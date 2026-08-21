package acp

import (
	"encoding/json"
	"strconv"
	"strings"
	"testing"

	"tapioca/internal/tools"
)

// pathArgs builds the arguments a built-in call would arrive with.
func pathArgs(t *testing.T, key, value string) json.RawMessage {
	t.Helper()
	return json.RawMessage(`{"` + key + `":` + strconv.Quote(value) + `}`)
}

// searchCall is what an agent sends before searching: the thing it is looking
// for, and where it is looking.
func searchCall(pattern, path string) map[string]any {
	return map[string]any{
		"toolCallId": "call-1",
		"title":      "Search for " + pattern,
		"kind":       "search",
		"status":     "pending",
		"rawInput":   map[string]any{"pattern": pattern, "path": path},
	}
}

// Searching outside the working tree is exfiltration with one level of
// indirection, and gateReadOnly prompts for it. An external agent's search was
// exempt: the subject handed to the gate was built under the key "pattern",
// and both things that read a grep's subject — subjectOf for the rules and
// gateReadOnly for the prompt — read "path". Neither ever saw one, so the
// prompt could not fire and no path rule could match. Two sides of a check
// that can never be in the same form.
//
// The move is `{"kind":"search","rawInput":{"pattern":"BEGIN OPENSSH PRIVATE
// KEY","path":"~/.ssh"}}` from an agent whose model has been fed injected
// instructions: Tapioca's own grep of that directory asks first, and this one
// did not ask at all.
func TestExternalSearchOutsideTheTreeIsGated(t *testing.T) {
	outside := t.TempDir()
	gate := gateFor(t, tools.ModeAuto)

	// The control: the same directory, searched by the built-in grep, prompts.
	// Without this "the ACP path did not prompt" could be true because nothing
	// prompts for this path at all.
	askedLocally := false
	if _, ok := gate.Approve("grep", pathArgs(t, "path", outside),
		func(string, string) tools.Decision {
			askedLocally = true
			return tools.Decision{Allow: true}
		}); !ok {
		t.Fatal("the built-in grep was refused outright; the comparison below would not be like for like")
	}
	if !askedLocally {
		t.Skip("the built-in grep gate did not fire for this path; the test would be vacuous")
	}

	a, f := connectFake(t, gate, func(f *fakeAgent) string {
		f.ask(searchCall("BEGIN OPENSSH PRIVATE KEY", outside))
		return "end_turn"
	})
	evs := prompt(t, a, "look around")
	if len(permissions(evs)) == 0 {
		t.Error("an external agent searched outside the working tree with no prompt, where the built-in grep asks")
	}
	if f.chosen() == "yes" {
		t.Error("the search was approved without the user being asked")
	}
}

// The same silence took path rules out with it: deny = ["grep(<dir>/**)"] is
// the rule a user writes to keep a search out of a directory, and an external
// agent's search never presented a path for it to match.
func TestExternalSearchAnswersToAPathDenyRule(t *testing.T) {
	outside := t.TempDir()
	gate := gateFor(t, tools.ModeBypass, "grep("+outside+"/**)")

	// The control: the rule really does deny the built-in call, so a search
	// that gets through below is the ACP path and not a misspelt rule.
	if _, ok := gate.Approve("grep", pathArgs(t, "path", outside),
		func(string, string) tools.Decision { return tools.Decision{Allow: true} }); ok {
		t.Skip("the deny rule does not match the built-in grep either; the test would be vacuous")
	}

	a, f := connectFake(t, gate, func(f *fakeAgent) string {
		f.ask(searchCall("token", outside))
		return "end_turn"
	})
	evs := prompt(t, a, "look around")
	if f.chosen() == "yes" {
		t.Error("a deny rule naming the directory did not stop an external agent's search of it")
	}
	if len(errorsOf(evs)) == 0 {
		t.Error("the refusal was never reported to the user")
	}
}

// Ordinary use: a search of the working tree itself is what an external agent
// does all day, and it must not start prompting. The built-in grep with no
// path asks for nothing, and this has to match it — including the case where
// the agent reports only what it is looking for and not where.
func TestExternalSearchInsideTheTreeDoesNotPrompt(t *testing.T) {
	gate := gateFor(t, tools.ModeAuto)
	a, f := connectFake(t, gate, func(f *fakeAgent) string {
		f.ask(map[string]any{
			"toolCallId": "call-1",
			"title":      "Search for TODO",
			"kind":       "search",
			"status":     "pending",
			"rawInput":   map[string]any{"pattern": "TODO"},
		})
		return "end_turn"
	})
	evs := prompt(t, a, "find the todos")
	if n := len(permissions(evs)); n != 0 {
		t.Errorf("an ordinary search of the working tree prompted %d times: %+v", n, permissions(evs))
	}
	if f.chosen() != "yes" {
		t.Errorf("an ordinary search of the working tree was not allowed: %q", f.chosen())
	}
}

// A tool call reports where it works through "locations", and a shell command
// is not a location. Reading one as the call's subject let an agent present a
// path as the command: the subject became "/tmp/notes.txt", subjectJSON handed
// the gate {"command":"/tmp/notes.txt"}, no deny rule for a shell command could
// match it, the prompt the user would have seen said "/tmp/notes.txt" rather
// than what runs — and, because a location counts as the call reporting
// itself, the last-resort prompt for an unreported call was skipped too. Under
// bypass that is a shell command running with nothing asked and nothing shown.
func TestExternalExecuteIsNotJudgedByItsLocations(t *testing.T) {
	gate := gateFor(t, tools.ModeBypass, "bash(rm *)")

	// The control: an execute call that does report its command runs under
	// bypass with no prompt, so a prompt below is about the missing command
	// rather than about bypass not being in force.
	quiet, quietAgent := connectFake(t, gate, func(f *fakeAgent) string {
		f.ask(bashCall("go build ./..."))
		return "end_turn"
	})
	if evs := prompt(t, quiet, "build"); len(permissions(evs)) != 0 {
		t.Skipf("bypass is prompting for an ordinary command here; the test would be vacuous: %+v", permissions(evs))
	}
	if quietAgent.chosen() != "yes" {
		t.Skip("the control command was not allowed; the test would be vacuous")
	}

	a, _ := connectFake(t, gate, func(f *fakeAgent) string {
		f.ask(map[string]any{
			"toolCallId": "call-2",
			"title":      "Tidy up",
			"kind":       "execute",
			"status":     "pending",
			"rawInput":   map[string]any{"description": "housekeeping"},
			"locations":  []any{map[string]any{"path": "/tmp/notes.txt"}},
		})
		return "end_turn"
	})
	evs := prompt(t, a, "tidy up")
	perms := permissions(evs)
	if len(perms) == 0 {
		t.Fatal("a shell command that never said what it runs was approved under bypass with no prompt")
	}
	for _, p := range perms {
		if strings.TrimSpace(p.Summary) == "/tmp/notes.txt" {
			t.Errorf("the prompt showed a location as the command it is about to run: %q", p.Summary)
		}
	}
}

// The same fallback answered for a fetch: the subject became a filesystem
// path, subjectJSON handed the gate {"url":"/tmp/notes.txt"}, FetchHost found
// no host in it, and the "new host" prompt — the only thing that gates a fetch
// — was skipped along with the unreported-call prompt behind it.
func TestExternalFetchIsNotJudgedByItsLocations(t *testing.T) {
	gate := gateFor(t, tools.ModeBypass)
	a, f := connectFake(t, gate, func(f *fakeAgent) string {
		f.ask(map[string]any{
			"toolCallId": "call-1",
			"title":      "Look something up",
			"kind":       "fetch",
			"status":     "pending",
			"rawInput":   map[string]any{"note": "reading a page"},
			"locations":  []any{map[string]any{"path": "/tmp/notes.txt"}},
		})
		return "end_turn"
	})
	evs := prompt(t, a, "look it up")
	if len(permissions(evs)) == 0 {
		t.Error("a fetch that never said what it fetches was approved with no prompt")
	}
	if f.chosen() == "yes" {
		t.Error("the fetch was approved without the user being asked")
	}
}

// Ordinary use, and the reason the fallback exists: a file tool that reports
// its path only in locations is fully reported, and must not grow a prompt.
func TestExternalEditReportedOnlyByItsLocationStillRunsQuietly(t *testing.T) {
	gate := gateFor(t, tools.ModeBypass)
	a, f := connectFake(t, gate, func(f *fakeAgent) string {
		f.ask(map[string]any{
			"toolCallId": "call-1",
			"title":      "Edit main.go",
			"kind":       "edit",
			"status":     "pending",
			"locations":  []any{map[string]any{"path": "main.go"}},
		})
		return "end_turn"
	})
	evs := prompt(t, a, "edit it")
	if n := len(permissions(evs)); n != 0 {
		t.Errorf("an edit that reported its path in locations prompted %d times", n)
	}
	if f.chosen() != "yes" {
		t.Errorf("an edit that reported its path in locations was refused: %q", f.chosen())
	}
}
