package tools

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// hooked returns an executor with these hooks in force, failing the test if
// any of them was refused.
func hooked(t *testing.T, mode string, hs ...Hook) *Executor {
	t.Helper()
	e := execIn(t, mode)
	if notes := e.SetHooks(hs); len(notes) > 0 {
		t.Fatalf("hooks refused: %v", notes)
	}
	return e
}

// The whole point of the feature: a hook that can only observe is a log, one
// that can refuse is a policy the user wrote.
func TestPreToolHookBlocksTheCallWithItsStderr(t *testing.T) {
	e := hooked(t, ModeBypass, Hook{
		Event:   HookPreTool,
		Match:   "write_file",
		Command: `echo "vendor/ is generated, edit the template" >&2; exit 1`,
	})
	res, err := e.CallDetailed(context.Background(), "write_file",
		args(t, map[string]string{"path": "vendor/x.go", "content": "package x"}),
		asker(Decision{Allow: true}, new([]string)))
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsErr || !strings.Contains(res.Text, "vendor/ is generated, edit the template") {
		t.Fatalf("call not refused with the hook's reason: %q", res.Text)
	}
	if _, err := os.Stat(filepath.Join(e.Cwd(), "vendor", "x.go")); !os.IsNotExist(err) {
		t.Fatal("the file was written anyway")
	}
}

// A hook narrows, never widens: a denied call must stay denied, and must not
// even reach a hook — otherwise "run this before every write" quietly becomes a
// way to act on the writes the rules stopped.
func TestHookCannotOverrideADenyRule(t *testing.T) {
	e := execIn(t, ModeBypass)
	marker := filepath.Join(e.Cwd(), "hook-ran")
	e.SetRules(nil, nil, []string{"write_file(**/*.go)"})
	if notes := e.SetHooks([]Hook{{Event: HookPreTool, Command: "touch '" + marker + "'"}}); len(notes) > 0 {
		t.Fatalf("hooks refused: %v", notes)
	}
	res, err := e.CallDetailed(context.Background(), "write_file",
		args(t, map[string]string{"path": "main.go", "content": "package main"}),
		asker(Decision{Allow: true}, new([]string)))
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsErr {
		t.Fatal("the deny rule stopped holding once a hook existed")
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("a hook ran for a call a rule had already denied")
	}
	if _, err := os.Stat(filepath.Join(e.Cwd(), "main.go")); !os.IsNotExist(err) {
		t.Fatal("the file was written anyway")
	}
}

// Exiting 0 is not permission: the prompt the mode calls for still happens, and
// the answer still decides.
func TestPreToolHookDoesNotSkipThePrompt(t *testing.T) {
	e := hooked(t, ModeManual, Hook{Event: HookPreTool, Command: "exit 0"})
	var asked []string
	res, err := e.CallDetailed(context.Background(), "write_file",
		args(t, map[string]string{"path": "main.go", "content": "package main"}),
		asker(Decision{}, &asked))
	if err != nil {
		t.Fatal(err)
	}
	if len(asked) != 1 {
		t.Fatalf("a hook exiting 0 replaced the prompt: asked=%v", asked)
	}
	if !res.IsErr {
		t.Fatal("the user's refusal was overridden")
	}
	if _, err := os.Stat(filepath.Join(e.Cwd(), "main.go")); !os.IsNotExist(err) {
		t.Fatal("the file was written anyway")
	}
}

// A policy that cannot run must refuse rather than wave the call through.
func TestPreToolHookThatCannotRunFailsClosed(t *testing.T) {
	e := hooked(t, ModeBypass, Hook{Event: HookPreTool, Command: "tapioca-no-such-hook-command"})
	res, err := e.CallDetailed(context.Background(), "write_file",
		args(t, map[string]string{"path": "main.go", "content": "package main"}),
		asker(Decision{Allow: true}, new([]string)))
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsErr {
		t.Fatalf("a hook that could not run let the call through: %q", res.Text)
	}
	if _, err := os.Stat(filepath.Join(e.Cwd(), "main.go")); !os.IsNotExist(err) {
		t.Fatal("the file was written anyway")
	}
}

// A hanging hook must not wedge the session.
func TestPreToolHookTimeoutBlocksInsteadOfHanging(t *testing.T) {
	e := hooked(t, ModeBypass, Hook{
		Event:   HookPreTool,
		Command: "sleep 60",
		Timeout: 200 * time.Millisecond,
	})
	start := time.Now()
	res, err := e.CallDetailed(context.Background(), "write_file",
		args(t, map[string]string{"path": "main.go", "content": "package main"}),
		asker(Decision{Allow: true}, new([]string)))
	if err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(start); elapsed > 30*time.Second {
		t.Fatalf("the call waited %s for a hook with a 200ms deadline", elapsed)
	}
	if !res.IsErr || !strings.Contains(res.Text, "timed out") {
		t.Fatalf("a timed-out hook did not refuse the call: %q", res.Text)
	}
}

// match names a tool, so a hook aimed at one must not fire for another.
func TestHookMatchSelectsTheTool(t *testing.T) {
	e := hooked(t, ModeBypass, Hook{Event: HookPreTool, Match: "bash", Command: "exit 1"})
	res, err := e.CallDetailed(context.Background(), "write_file",
		args(t, map[string]string{"path": "main.go", "content": "package main"}),
		asker(Decision{Allow: true}, new([]string)))
	if err != nil {
		t.Fatal(err)
	}
	if res.IsErr {
		t.Fatalf("a bash hook blocked a write: %q", res.Text)
	}
	res, err = e.CallDetailed(context.Background(), "bash",
		args(t, map[string]string{"command": "echo hi"}), asker(Decision{Allow: true}, new([]string)))
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsErr {
		t.Fatalf("the bash hook did not fire: %q", res.Text)
	}
}

// "A post_tool hook sees which tool ran and with what": the environment names
// the call and stdin carries the exact arguments.
func TestPostToolHookSeesTheCall(t *testing.T) {
	e := execIn(t, ModeBypass)
	log := filepath.Join(e.Cwd(), "hook.log")
	if notes := e.SetHooks([]Hook{{
		Event:   HookPostTool,
		Match:   "edit_*",
		Command: `{ echo "$TAPIOCA_EVENT $TAPIOCA_TOOL $TAPIOCA_TOOL_ERROR $TAPIOCA_TOOL_PATH"; cat; } > '` + log + `'`,
	}}); len(notes) > 0 {
		t.Fatalf("hooks refused: %v", notes)
	}
	target := filepath.Join(e.Cwd(), "main.go")
	if err := os.WriteFile(target, []byte("package main // old\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := e.CallDetailed(context.Background(), "edit_file",
		args(t, map[string]string{"path": "main.go", "old_string": "old", "new_string": "new"}),
		asker(Decision{Allow: true}, new([]string))); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(log)
	if err != nil {
		t.Fatalf("the post_tool hook did not run: %v", err)
	}
	got := string(data)
	if !strings.HasPrefix(got, "post_tool edit_file 0 "+target+"\n") {
		t.Fatalf("hook environment described the call as %q", got)
	}
	if !strings.Contains(got, `"old_string":"old"`) {
		t.Fatalf("the arguments did not reach the hook's stdin: %q", got)
	}
}

// A post_tool hook cannot unrun the call, but a formatter that failed is
// something the model has to be told about.
func TestPostToolHookFailureIsReportedNotFatal(t *testing.T) {
	e := hooked(t, ModeBypass, Hook{
		Event:   HookPostTool,
		Command: `echo "gofmt: command not found" >&2; exit 127`,
	})
	res, err := e.CallDetailed(context.Background(), "write_file",
		args(t, map[string]string{"path": "main.go", "content": "package main"}),
		asker(Decision{Allow: true}, new([]string)))
	if err != nil {
		t.Fatal(err)
	}
	if res.IsErr {
		t.Fatalf("a failing post_tool hook failed the call: %q", res.Text)
	}
	if !strings.Contains(res.Text, "post_tool hook") || !strings.Contains(res.Text, "gofmt: command not found") {
		t.Fatalf("the failure was not reported: %q", res.Text)
	}
	if _, err := os.Stat(filepath.Join(e.Cwd(), "main.go")); err != nil {
		t.Fatalf("the write did not happen: %v", err)
	}
}

// Hooks run user commands, and a leaked provider key funds someone else's
// inference. They get the environment bash gets.
func TestHookEnvironmentHasNoProviderKeys(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "sk-ant-must-not-leak")
	e := execIn(t, ModeBypass)
	dump := filepath.Join(e.Cwd(), "env.txt")
	if notes := e.SetHooks([]Hook{{Event: HookPreTool, Command: "env > '" + dump + "'"}}); len(notes) > 0 {
		t.Fatalf("hooks refused: %v", notes)
	}
	if _, err := e.CallDetailed(context.Background(), "bash",
		args(t, map[string]string{"command": "echo hi"}), asker(Decision{Allow: true}, new([]string))); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(dump)
	if err != nil {
		t.Fatalf("the hook did not run: %v", err)
	}
	if !strings.Contains(string(data), "TAPIOCA_TOOL=bash") {
		t.Fatalf("the hook saw no environment at all, so this proves nothing: %q", data)
	}
	if strings.Contains(string(data), "sk-ant-must-not-leak") {
		t.Fatal("a provider key reached a hook")
	}
}

func TestSessionHooksRun(t *testing.T) {
	e := hooked(t, ModeBypass,
		Hook{Event: HookSessionStart, Command: `echo "$TAPIOCA_EVENT" > started`},
		Hook{Event: HookSessionEnd, Command: `echo "$TAPIOCA_EVENT" > ended`},
		Hook{Event: HookSessionEnd, Command: `echo "no notifier here" >&2; exit 1`},
	)
	if notes := e.RunSessionHooks(HookSessionStart); len(notes) > 0 {
		t.Fatalf("session_start reported %v", notes)
	}
	if data, err := os.ReadFile(filepath.Join(e.Cwd(), "started")); err != nil || strings.TrimSpace(string(data)) != "session_start" {
		t.Fatalf("session_start hook: %q %v", data, err)
	}
	// One failing hook is reported and the other still runs: a broken
	// notification must not swallow the logging next to it.
	notes := e.RunSessionHooks(HookSessionEnd)
	if len(notes) != 1 || !strings.Contains(notes[0], "no notifier here") {
		t.Fatalf("session_end reported %v", notes)
	}
	if data, err := os.ReadFile(filepath.Join(e.Cwd(), "ended")); err != nil || strings.TrimSpace(string(data)) != "session_end" {
		t.Fatalf("session_end hook: %q %v", data, err)
	}
}

// A hook naming an event that does not exist would otherwise be a policy its
// author believes is in force and that never fires.
func TestSetHooksRefusesWhatCannotFire(t *testing.T) {
	e := execIn(t, ModeBypass)
	notes := e.SetHooks([]Hook{
		{Event: "pre_todo", Command: "echo x"},
		{Event: HookPreTool, Command: "   "},
		{Event: HookPreTool, Command: "echo ok"},
	})
	if len(notes) != 2 {
		t.Fatalf("complaints: %v", notes)
	}
	if hooks := e.Hooks(); len(hooks) != 1 || hooks[0].Command != "echo ok" {
		t.Fatalf("kept %+v", hooks)
	}
}

// A hook is a subprocess in the critical path of every call, so its deadline is
// bounded no matter what the config says.
func TestHookTimeoutIsCapped(t *testing.T) {
	e := hooked(t, ModeBypass, Hook{Event: HookPreTool, Command: "true", Timeout: time.Hour})
	if got := e.Hooks()[0].Timeout; got != maxHookTimeout {
		t.Fatalf("timeout %s, want %s", got, maxHookTimeout)
	}
}
