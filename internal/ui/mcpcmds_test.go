package ui

import (
	"testing"

	"tapioca/internal/agent"
	"tapioca/internal/mcp"
)

func TestPromptArgsMapping(t *testing.T) {
	one := mcp.Prompt{Arguments: []mcp.PromptArg{{Name: "path"}}}
	// A single argument takes the whole line, spaces and all.
	if got := promptArgs(one, "internal/ui/app.go and hurry"); got["path"] != "internal/ui/app.go and hurry" {
		t.Fatalf("single argument: %+v", got)
	}
	if got := promptArgs(one, ""); got != nil {
		t.Fatalf("empty input produced %+v", got)
	}

	two := mcp.Prompt{Arguments: []mcp.PromptArg{{Name: "file"}, {Name: "message"}}}
	got := promptArgs(two, "main.go be thorough about it")
	if got["file"] != "main.go" || got["message"] != "be thorough about it" {
		t.Fatalf("the last argument should keep its spaces: %+v", got)
	}

	if got := promptArgs(mcp.Prompt{}, "whatever"); got != nil {
		t.Fatalf("a prompt taking no arguments got %+v", got)
	}
}

// @ tokens are paths far more often than resources, and a path may contain a
// colon, so a token only becomes a resource when its prefix names a server we
// are actually connected to.
func TestMentionWithAColonIsNotAResource(t *testing.T) {
	m := &App{mgr: &agent.Manager{MCP: mcp.NewRegistry()}}
	for _, token := range []string{"notes/todo:2.md", "C:/tmp/x", "db://users", "plain.go"} {
		if _, ok := m.readMCPResource(token); ok {
			t.Errorf("%q was taken for an MCP resource with no server connected", token)
		}
	}
}

// With no registry at all, nothing here may panic — the UI runs fine without
// a single MCP server.
func TestMCPHelpersTolerateNoServers(t *testing.T) {
	m := &App{mgr: &agent.Manager{}}
	if got := m.mcpPromptCmds(); got != nil {
		t.Fatalf("prompts without a registry: %+v", got)
	}
	if got := m.mcpResourceMatches("x"); got != nil {
		t.Fatalf("resources without a registry: %+v", got)
	}
	if _, ok := m.readMCPResource("s:uri"); ok {
		t.Fatal("resolved a resource without a registry")
	}
	if _, ok := m.findMCPPrompt("anything"); ok {
		t.Fatal("found a prompt without a registry")
	}
}
