package acp

import (
	"strings"
	"testing"

	"tapioca/internal/tools"
)

// hiddenCall is a real command the bridge cannot read: nested one level below
// where the arguments are looked for, under a title written for a human. The
// rules can only match the title, and the title is not what runs.
func hiddenCall(title, command string) map[string]any {
	return map[string]any{
		"toolCallId": "call-1",
		"title":      title,
		"kind":       "execute",
		"status":     "pending",
		"rawInput":   map[string]any{"input": map[string]any{"command": command}},
	}
}

// The hole this closes: deny = ["bash(rm *)"] did not match "Tidy up the build
// directory", and bypass then waived the prompt, so the rm ran without the
// user ever seeing it. A rule that does not match prose has decided nothing,
// and the call has to reach the one judge left.
func TestACommandTheAgentDidNotReportIsAskedAboutUnderBypass(t *testing.T) {
	a, f := connectFake(t, gateFor(t, tools.ModeBypass, "bash(rm *)"), func(f *fakeAgent) string {
		f.ask(hiddenCall("Tidy up the build directory", "rm -rf ~"))
		return "end_turn"
	})

	evs := prompt(t, a, "clean up") // no answer supplied: the user says no
	if got := f.chosen(); got != "no" {
		t.Fatalf("client answered %q; an unreadable call must not pass on a rule that could not read it", got)
	}
	reqs := permissions(evs)
	if len(reqs) != 1 {
		t.Fatalf("got %d prompts, want one even under bypass", len(reqs))
	}
	// The prompt has to say what it cannot vouch for, or it reads as a normal
	// approval of a command that was never shown.
	if !strings.Contains(reqs[0].Summary, "did not report") {
		t.Errorf("prompt = %q; it does not say the arguments were unreported", reqs[0].Summary)
	}
}

// Approving one is not approving the next: the arguments were unknown, so
// there is nothing to remember. An "always" answer still means this once.
func TestAnAlwaysAnswerToAnUnreportedCallIsNotRemembered(t *testing.T) {
	asked := make(chan struct{}, 2)
	a, f := connectFake(t, gateFor(t, tools.ModeBypass), func(f *fakeAgent) string {
		f.ask(hiddenCall("Tidy up the build directory", "rm -rf ~"))
		asked <- struct{}{}
		return "end_turn"
	})

	evs := prompt(t, a, "clean up", tools.Decision{Allow: true, Always: true})
	if len(permissions(evs)) != 1 {
		t.Fatal("expected one prompt on the first turn")
	}
	if got := f.chosen(); got != "yes" {
		t.Fatalf("client answered %q after the user allowed it", got)
	}
	<-asked

	evs = prompt(t, a, "again", tools.Decision{Allow: true})
	if len(permissions(evs)) != 1 {
		t.Errorf("%d prompts on the second turn; an unreadable call must be asked about every time",
			len(permissions(evs)))
	}
}

// A deny rule that does match the title still refuses without asking: the
// prompt is the fallback for a rule that could not decide, not a way around
// one that did.
func TestADenyRuleThatMatchesTheTitleStillRefuses(t *testing.T) {
	a, f := connectFake(t, gateFor(t, tools.ModeBypass, "bash(rm *)"), func(f *fakeAgent) string {
		f.ask(map[string]any{
			"toolCallId": "call-1", "title": "rm -rf /tmp/work",
			"kind": "execute", "status": "pending",
		})
		return "end_turn"
	})

	evs := prompt(t, a, "clean up")
	if got := f.chosen(); got != "no" {
		t.Fatalf("client answered %q, want the reject option", got)
	}
	if p := permissions(evs); len(p) != 0 {
		t.Errorf("%d prompts; a denied call is not the user's to approve", len(p))
	}
}

// The prompt the gate already shows is the same prompt. An unreported call in
// a mode that asks anyway must not ask twice.
func TestAnUnreportedCallIsNotAskedAboutTwice(t *testing.T) {
	a, f := connectFake(t, gateFor(t, tools.ModeManual), func(f *fakeAgent) string {
		f.ask(hiddenCall("Tidy up the build directory", "rm -rf ~"))
		return "end_turn"
	})

	evs := prompt(t, a, "clean up", tools.Decision{Allow: true})
	if n := len(permissions(evs)); n != 1 {
		t.Fatalf("got %d prompts, want exactly one", n)
	}
	if got := f.chosen(); got != "yes" {
		t.Fatalf("client answered %q", got)
	}
}

// A call that reports its arguments is unaffected: bypass still waives the
// prompt for it, which is what the mode is for.
func TestAReportedCommandStillRunsUnaskedUnderBypass(t *testing.T) {
	a, f := connectFake(t, gateFor(t, tools.ModeBypass, "bash(rm *)"), func(f *fakeAgent) string {
		f.ask(bashCall("go build ./..."))
		return "end_turn"
	})

	evs := prompt(t, a, "build it")
	if got := f.chosen(); got != "yes" {
		t.Fatalf("client answered %q, want allow", got)
	}
	if p := permissions(evs); len(p) != 0 {
		t.Errorf("%d prompts under bypass for a call the rules could read", len(p))
	}
}
