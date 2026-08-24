package config

import (
	"os"
	"path/filepath"
	"testing"
)

func writeAt(t *testing.T, p, s string) {
	t.Helper()
	os.MkdirAll(filepath.Dir(p), 0o755)
	if err := os.WriteFile(p, []byte(s), 0o644); err != nil {
		t.Fatal(err)
	}
}

// The user's own config, in a directory that is not a checkout.
func TestOwnConfigIsNotRestricted(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	pretendHome(t, home)
	path := filepath.Join(home, ".config", "tapioca", "config.toml")
	writeAt(t, path, "")

	for _, cwd := range []string{home, filepath.Dir(home)} {
		c := Default()
		c.path = path
		c.MCP = []MCPServerConfig{{Name: "s", Command: "sh"}}
		c.PermissionMode = "auto"
		if notes := c.RestrictIfInsideTree(cwd); notes != nil {
			t.Errorf("cwd %s: the user's own config was restricted: %v", cwd, notes)
		}
		if len(c.MCP) != 1 || c.PermissionMode != "auto" {
			t.Errorf("cwd %s: config was stripped", cwd)
		}
	}
}

// A dotfiles repo in the home directory is common and must not make the
// config that sits beside it untrusted.
func TestDotfilesRepoDoesNotRestrictOwnConfig(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	pretendHome(t, home)
	os.MkdirAll(filepath.Join(home, ".git"), 0o755)
	path := filepath.Join(home, ".config", "tapioca", "config.toml")
	writeAt(t, path, "")
	work := filepath.Join(home, "project")
	os.MkdirAll(filepath.Join(work, ".git"), 0o755)

	c := Default()
	c.path = path
	c.MCP = []MCPServerConfig{{Name: "s", Command: "sh"}}
	if notes := c.RestrictIfInsideTree(work); notes != nil {
		t.Errorf("a dotfiles repo restricted the user's own config: %v", notes)
	}
}

// The attack the file's own comment names: an .envrc points XDG_CONFIG_HOME
// into the checkout, so the config read at launch is one the archive wrote.
func TestConfigInsideCheckoutIsRestricted(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	tree := filepath.Join(home, "checkout")
	os.MkdirAll(filepath.Join(tree, ".git"), 0o755)
	path := filepath.Join(tree, ".config", "tapioca", "config.toml")
	writeAt(t, path, "")

	c := Default()
	c.path = path
	c.MCP = []MCPServerConfig{{Name: "s", Command: "sh"}}
	c.LSP = []LSPServerConfig{{Command: "sh"}}
	c.PermissionMode = "bypass"
	notes := c.RestrictIfInsideTree(tree)
	if len(notes) == 0 {
		t.Fatal("a config committed inside the checkout was trusted")
	}
	if len(c.MCP) != 0 || len(c.LSP) != 0 || c.PermissionMode != "manual" {
		t.Errorf("not restricted: mcp=%d lsp=%d mode=%s", len(c.MCP), len(c.LSP), c.PermissionMode)
	}
}

// The archive chooses where its markers are: a go.mod in the subdirectory
// being worked in must not make a config one level above it "outside".
func TestMarkerInSubdirectoryDoesNotEscape(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	tree := filepath.Join(home, "checkout")
	os.MkdirAll(filepath.Join(tree, ".git"), 0o755)
	path := filepath.Join(tree, ".config", "tapioca", "config.toml")
	writeAt(t, path, "")
	sub := filepath.Join(tree, "sub")
	writeAt(t, filepath.Join(sub, "go.mod"), "module x\n")

	c := Default()
	c.path = path
	c.MCP = []MCPServerConfig{{Name: "s", Command: "sh"}}
	if notes := c.RestrictIfInsideTree(sub); len(notes) == 0 {
		t.Fatal("a marker in the subdirectory moved the boundary the archive's way")
	}
}
