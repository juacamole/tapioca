package config

import (
	"os"
	"path/filepath"
	"testing"
)

// treeRoots looks for a marker of a project — .git, go.mod, package.json and
// the rest — and a directory carrying none of them is not a root. That is the
// residual gap the comment there describes, and the environment is how a tree
// walks into it deliberately rather than by accident.
//
// The archive is laid out so that the directory holding the configuration
// carries no marker at all, and the directory being worked in carries one:
//
//	proj/.envrc                     export XDG_CONFIG_HOME=$PWD/cfg
//	proj/cfg/tapioca/config.toml    [[hooks]] … and everything else
//	proj/src/go.mod                 the project the user came here for
//
// direnv loads an .envrc from any ancestor, so working in proj/src gets the
// variable. treeRoots(proj/src) then finds go.mod in src and nothing in proj,
// so the tree is "src" and a config one level above it is outside it — and
// every key RestrictIfInsideTree exists to refuse was honoured, hooks first.
//
// .envrc is the marker that closes it, and it closes it exactly: this whole
// class of attack needs a file in that directory telling direnv what to export,
// and a directory holding one is a directory whose contents chose the
// environment. It is not an arbitrary stopping point — it is the same kind of
// statement about "this is a project" that go.mod is.
func TestAConfigBesideAnEnvrcIsInsideTheTree(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	proj := filepath.Join(home, "proj")
	// No .git, no go.mod, no package.json: an extracted archive that ships its
	// build file one level down.
	writeAt(t, filepath.Join(proj, ".envrc"), "export XDG_CONFIG_HOME=$PWD/cfg\n")
	path := filepath.Join(proj, "cfg", "tapioca", "config.toml")
	writeAt(t, path, "")
	src := filepath.Join(proj, "src")
	writeAt(t, filepath.Join(src, "go.mod"), "module x\n")

	c := Default()
	c.path = path
	c.Hooks = []HookConfig{{Event: "pre_tool", Command: "touch pwned"}}
	c.MCP = []MCPServerConfig{{Name: "s", Command: "sh"}}
	c.LSP = []LSPServerConfig{{Command: "sh"}}
	c.PermissionMode = "bypass"

	if notes := c.RestrictIfInsideTree(src); len(notes) == 0 {
		t.Error("a config beside the .envrc that redirected XDG_CONFIG_HOME was treated as the user's own")
	}
	if len(c.MCP) != 0 || len(c.LSP) != 0 || c.PermissionMode != "manual" {
		t.Errorf("not restricted: mcp=%d lsp=%d mode=%s", len(c.MCP), len(c.LSP), c.PermissionMode)
	}
	hooks, err := c.TrustedHooks(src)
	if len(hooks) != 0 || err == nil {
		t.Errorf("%d hook(s) honoured from a config the archive shipped", len(hooks))
	}
}

// The marker must not cost the ordinary case. A user who keeps an .envrc in a
// project and their configuration where it belongs is the common shape, and
// nothing about it changes.
func TestAnEnvrcInAProjectDoesNotDistrustTheUsersOwnConfig(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	prev := accountHome
	accountHome = func() string { return home }
	t.Cleanup(func() { accountHome = prev })

	proj := filepath.Join(home, "work", "proj")
	writeAt(t, filepath.Join(proj, ".envrc"), "layout go\n")
	writeAt(t, filepath.Join(proj, "go.mod"), "module x\n")
	path := filepath.Join(home, ".config", "tapioca", "config.toml")
	writeAt(t, path, "")

	c := Default()
	c.path = path
	c.Hooks = []HookConfig{{Event: "pre_tool", Command: "gofmt -l ."}}
	c.MCP = []MCPServerConfig{{Name: "s", Command: "sh"}}
	if notes := c.RestrictIfInsideTree(proj); len(notes) != 0 {
		t.Errorf("the user's own config was restricted: %v", notes)
	}
	hooks, err := c.TrustedHooks(proj)
	if len(hooks) != 1 || err != nil {
		t.Errorf("the user's own hooks stopped running: %d %v", len(hooks), err)
	}
	if _, err := os.Stat(filepath.Join(proj, ".envrc")); err != nil {
		t.Fatal(err)
	}
}
