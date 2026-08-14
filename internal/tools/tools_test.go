package tools

import (
	"context"
	"encoding/json"
	"testing"
)

func denyAll(t *testing.T, asked *[]string) AskFunc {
	return func(tool, summary string) Decision {
		*asked = append(*asked, summary)
		return Decision{}
	}
}

func TestPlanModeIgnoresGrants(t *testing.T) {
	e := NewExecutor(t.TempDir(), ModePlan)
	e.SetBashPrefixes([]string{"git"})
	e.GrantExternal("bash")

	var asked []string
	out, isErr, err := e.Call(context.Background(), "bash",
		json.RawMessage(`{"command":"git commit -m x"}`), denyAll(t, &asked))
	if err != nil {
		t.Fatal(err)
	}
	if len(asked) == 0 {
		t.Fatal("plan mode let a granted command through without asking")
	}
	if !isErr || out == "" {
		t.Fatalf("denied call should error: %q", out)
	}
}

func TestPlanModeBlocksFileEdits(t *testing.T) {
	e := NewExecutor(t.TempDir(), ModePlan)
	var asked []string
	out, isErr, _ := e.Call(context.Background(), "write_file",
		json.RawMessage(`{"path":"x","content":"y"}`), denyAll(t, &asked))
	if !isErr {
		t.Fatalf("plan mode allowed write_file: %q", out)
	}
	if len(asked) != 0 {
		t.Fatal("write_file in plan mode should hard-deny, not ask")
	}
}

func TestSegmentsSplitCompound(t *testing.T) {
	got := segments("ls -la || pwd && whoami")
	if len(got) != 3 || got[0] != "ls -la" || got[1] != "pwd" || got[2] != "whoami" {
		t.Fatalf("bad split: %#v", got)
	}
}

// Approve is the gate on its own, for callers that run the work elsewhere.
// What it decides has to be what CallDetailed would have acted on.
func TestApproveDecidesWithoutRunning(t *testing.T) {
	dir := t.TempDir()
	e := NewExecutor(dir, ModeBypass)
	e.SetRules(nil, nil, []string{"bash(rm *)"})

	var asked []string
	if denial, ok := e.Approve("bash", json.RawMessage(`{"command":"rm -rf /"}`), denyAll(t, &asked)); ok {
		t.Fatal("a deny rule did not hold under bypass")
	} else if denial == "" {
		t.Fatal("a refusal with nothing to report back to the model")
	}
	if len(asked) != 0 {
		t.Fatal("a denied call was put in front of the user anyway")
	}
	if _, ok := e.Approve("bash", json.RawMessage(`{"command":"ls"}`), denyAll(t, &asked)); !ok {
		t.Fatal("bypass refused a command no rule mentions")
	}
}

// The keyed gate answers for tools Tapioca does not implement, and it is the
// same answer whether an MCP server or an external agent is asking.
func TestApproveExternalFollowsRulesAndGrants(t *testing.T) {
	e := NewExecutor(t.TempDir(), ModeBypass)
	e.SetRules(nil, []string{"acp:other"}, []string{"mcp:*__delete_*"})

	var asked []string
	if _, ok := e.ApproveExternal("mcp:fs__delete_file", `{"path":"x"}`, denyAll(t, &asked)); ok {
		t.Fatal("a deny rule did not hold under bypass")
	}
	if len(asked) != 0 {
		t.Fatal("a denied call was put in front of the user anyway")
	}
	// An ask rule outranks bypass and an earlier always-allow alike.
	e.GrantExternal("acp:other")
	if _, ok := e.ApproveExternal("acp:other", "{}", denyAll(t, &asked)); ok {
		t.Fatal("an ask rule was skipped for a granted tool")
	}
	if len(asked) != 1 {
		t.Fatalf("asked %d times, want the prompt the rule forces", len(asked))
	}
}

func TestExternalGate(t *testing.T) {
	e := NewExecutor(t.TempDir(), ModeManual)
	if e.ExternalAllowed("mcp:fs_write") {
		t.Fatal("ungranted external tool allowed in manual mode")
	}
	e.GrantExternal("mcp:fs_write")
	if !e.ExternalAllowed("mcp:fs_write") {
		t.Fatal("grant not honored")
	}
	e.SetMode(ModePlan)
	if e.ExternalAllowed("mcp:fs_write") {
		t.Fatal("plan mode honored an external grant")
	}
	e.SetMode(ModeBypass)
	if !e.ExternalAllowed("mcp:anything") {
		t.Fatal("bypass should allow externals")
	}
}
