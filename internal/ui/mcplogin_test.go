package ui

import (
	"testing"

	"tapioca/internal/agent"
	"tapioca/internal/config"
	"tapioca/internal/mcp"
)

// Logging in to the wrong server costs a consent screen and leaves the one that
// was meant as broken as it was, so the argument is only optional when there is
// nothing to be wrong about.
func TestPickMCPServer(t *testing.T) {
	one := []config.MCPServerConfig{{Name: "linear", URL: "https://mcp.linear.app/mcp", Auth: "oauth"}}
	several := append([]config.MCPServerConfig{}, one...)
	several = append(several, config.MCPServerConfig{Name: "My Notes", URL: "https://notes/mcp", Auth: "oauth"})

	cases := []struct {
		name    string
		servers []config.MCPServerConfig
		arg     string
		want    string
		wantOK  bool
	}{
		{"one server needs no argument", one, "", "linear", true},
		{"named", several, "linear", "linear", true},
		{"case does not matter", several, "LINEAR", "linear", true},
		{"the sanitized name works too", several, "My-Notes", "My Notes", true},
		{"several servers need an argument", several, "", "", false},
		{"an unknown name is not a guess", several, "notion", "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := pickMCPServer(c.servers, c.arg)
			if ok != c.wantOK {
				t.Fatalf("pickMCPServer(%q) ok = %v, want %v", c.arg, ok, c.wantOK)
			}
			if ok && got.Name != c.want {
				t.Errorf("pickMCPServer(%q) = %q, want %q", c.arg, got.Name, c.want)
			}
		})
	}
}

// /mcp with nothing to log in to must say so rather than open a browser.
func TestMCPLoginWithoutAnOAuthEntry(t *testing.T) {
	m := &App{cfg: &config.Config{MCP: []config.MCPServerConfig{
		{Name: "files", Command: "server"},
		{Name: "docs", URL: "https://docs/mcp", Headers: map[string]string{"Authorization": "Bearer x"}},
	}}, mgr: &agent.Manager{MCP: mcp.NewRegistry()}}
	cmdMCPLogin(m, "")
	if !m.flashErr {
		t.Fatalf("no oauth entry should be reported as a problem, got flash %q", m.flash)
	}
}
