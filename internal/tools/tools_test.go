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
