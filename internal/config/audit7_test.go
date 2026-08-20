package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A uid with no passwd entry is ordinary inside a container, so user.Current
// fails there and the exemption has no second opinion to check $HOME against.
// It has to stand anyway: telling every containerised user that their own
// config is untrusted is a policy they would turn off.
func TestNoAccountEntryKeepsTheUsersOwnConfigTrusted(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	old := accountHome
	accountHome = func() string { return "" }
	t.Cleanup(func() { accountHome = old })

	// A dotfiles repo, so the config really is inside a tree and the exemption
	// is the only thing that can save it.
	if err := os.MkdirAll(filepath.Join(home, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(home, ".config", "tapioca", "config.toml")
	writeAt(t, path, "")

	c := Default()
	c.path = path
	c.MCP = []MCPServerConfig{{Name: "s", Command: "sh"}}
	c.Hooks = []HookConfig{{Event: "session_start", Command: "true"}}
	if notes := c.RestrictIfInsideTree(home); notes != nil {
		t.Errorf("no passwd entry made the user's own config untrusted: %v", notes)
	}
	if _, err := c.TrustedHooks(home); err != nil {
		t.Errorf("no passwd entry refused the user's own hooks: %v", err)
	}
}

// XDG_CONFIG_HOME pointed at a versioned dotfiles directory is a real setup,
// and working inside that directory does put the config inside a tree — the
// refusal is correct. What was not correct was the advice: it named the path
// the file already had, so the one thing the user was told to do was a no-op.
func TestARefusalNeverSaysMoveTheFileToWhereItIs(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	pretendHome(t, home)
	dots := filepath.Join(home, "dotfiles")
	if err := os.MkdirAll(filepath.Join(dots, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_CONFIG_HOME", dots)
	path := filepath.Join(dots, "tapioca", "config.toml")
	writeAt(t, path, "")

	c := Default()
	c.path = path
	c.MCP = []MCPServerConfig{{Name: "s", Command: "sh"}}
	c.Hooks = []HookConfig{{Event: "session_start", Command: "true"}}

	notes := c.RestrictIfInsideTree(dots)
	if len(notes) == 0 {
		t.Fatal("control failed: a config inside a tree was not restricted at all")
	}
	_, err := c.TrustedHooks(dots)
	if err == nil {
		t.Fatal("control failed: hooks from a config inside a tree were honoured")
	}
	for _, msg := range append(notes, err.Error()) {
		if strings.Contains(msg, "move it to "+path) || strings.Contains(msg, "move them to "+path) {
			t.Errorf("the refusal tells the user to move the file to where it already is: %s", msg)
		}
	}
}

// Localhost stays legal for an in-tree config, because pointing at a model
// server on this machine is the one reason a repository has to name one. A LAN
// address is not this machine.
func TestOffMachineKnowsThisMachine(t *testing.T) {
	for _, c := range []struct {
		raw  string
		want bool
	}{
		{"http://localhost:11434", false},
		{"http://127.0.0.1:8080", false},
		{"http://[::1]:8080", false},
		{"http://localhost.:8080", false},
		{"http://127.0.0.2:8080", false},
		{"http://192.168.1.10:11434", true},
		{"http://10.0.0.5:8080", true},
		{"http://127.example.com", true},
		{"https://api.anthropic.com", true},
		{"", false},
	} {
		if got := offMachine(c.raw); got != c.want {
			t.Errorf("offMachine(%q) = %v, want %v", c.raw, got, c.want)
		}
	}
}

// --mcp-config names [[mcp]] servers, which are programs started at launch.
// It is a second way of pointing at a file the tree could have written, and
// the rule that refuses one has to refuse the other.
func TestAnMCPListInsideTheTreeIsJudgedLikeAConfig(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	tree := t.TempDir()
	if err := os.MkdirAll(filepath.Join(tree, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	inside := filepath.Join(tree, "mcp.toml")
	writeAt(t, inside, "[[mcp]]\nname = \"x\"\ncommand = \"sh\"\n")
	if !InsideTree(inside, tree) {
		t.Error("a file committed in the checkout was read as outside it")
	}
	if !InsideTree("./mcp.toml", tree) {
		t.Error("a relative path was read as outside the tree")
	}

	// Control: the same rule must not refuse a list the user keeps elsewhere.
	outside := filepath.Join(t.TempDir(), "mcp.toml")
	writeAt(t, outside, "")
	if InsideTree(outside, tree) {
		t.Error("a file outside the tree was refused")
	}
}
