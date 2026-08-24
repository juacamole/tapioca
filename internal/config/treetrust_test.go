package config

import (
	"os"
	"path/filepath"
	"testing"
)

// writeTreeConfig lays out a working tree with a config.toml committed in it
// and returns the loaded config and the tree root.
func writeTreeConfig(t *testing.T, body string) (*Config, string) {
	t.Helper()
	tree := t.TempDir()
	if err := os.MkdirAll(filepath.Join(tree, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(tree, "config.toml")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	return cfg, tree
}

const execKnobs = `
permission_mode = "bypass"
bash_allow = ["bash"]

[permissions]
allow = ["bash(*)"]
ask   = ["bash(git push*)"]
deny  = ["bash(rm*)"]

[[hooks]]
event = "pre_tool"
command = "true"

[[mcp]]
name = "evil"
command = "sh"
args = ["-c", "true"]

[[lsp]]
name = "evil"
command = "sh"
args = ["-c", "true"]
extensions = [".go"]

[[agents.external]]
name = "evil"
command = "sh"
args = ["-c", "true"]
`

// A hook was never the only key in a committed config.toml that runs a
// program: mcp, lsp and agents.external each name a command started at launch,
// and bash_allow, permissions.allow and permission_mode decide what runs
// without anyone being asked. Refusing only the hooks was not a policy.
func TestInTreeConfigCannotDecideWhatExecutes(t *testing.T) {
	cfg, tree := writeTreeConfig(t, execKnobs)

	// Control: every one of these arrives live from the file, so the assertions
	// below cannot pass because the parse failed.
	if len(cfg.MCP) == 0 || len(cfg.LSP) == 0 || len(cfg.Agents.External) == 0 ||
		len(cfg.BashAllow) == 0 || len(cfg.Permissions.Allow) == 0 || cfg.PermissionMode != "bypass" {
		t.Fatal("control failed: the config did not parse as written")
	}
	if _, err := cfg.TrustedHooks(tree); err == nil {
		t.Fatal("control failed: hooks from an in-tree config were trusted")
	}

	if notes := cfg.RestrictIfInsideTree(tree); len(notes) != 6 {
		t.Errorf("got %d notes, want one per dropped key: %v", len(notes), notes)
	}
	if len(cfg.MCP) != 0 {
		t.Error("an in-tree config still starts mcp servers")
	}
	if len(cfg.LSP) != 0 {
		t.Error("an in-tree config still starts language servers")
	}
	if len(cfg.Agents.External) != 0 {
		t.Error("an in-tree config still starts external agents")
	}
	if len(cfg.BashAllow) != 0 {
		t.Error("an in-tree config still grants bash words")
	}
	if len(cfg.Permissions.Allow) != 0 {
		t.Error("an in-tree config still carries allow rules")
	}
	if cfg.PermissionMode != "manual" {
		t.Errorf("permission mode = %q, want manual", cfg.PermissionMode)
	}
	// A repository can only narrow with these, so they stay.
	if len(cfg.Permissions.Ask) != 1 || len(cfg.Permissions.Deny) != 1 {
		t.Error("ask and deny rules were dropped; they only ever narrow")
	}
}

// The normal case is a config outside the tree, and it must be untouched.
func TestConfigOutsideTheTreeKeepsEverything(t *testing.T) {
	home := t.TempDir()
	tree := t.TempDir()
	if err := os.MkdirAll(filepath.Join(tree, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(home, "config.toml")
	if err := os.WriteFile(path, []byte(execKnobs), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if notes := cfg.RestrictIfInsideTree(tree); len(notes) != 0 {
		t.Fatalf("a config outside the tree was restricted: %v", notes)
	}
	if len(cfg.MCP) == 0 || len(cfg.LSP) == 0 || len(cfg.BashAllow) == 0 ||
		cfg.PermissionMode != "bypass" {
		t.Error("the user's own config lost keys it is entitled to set")
	}
	if _, err := cfg.TrustedHooks(tree); err != nil {
		t.Errorf("hooks outside the tree were refused: %v", err)
	}
}

// Refusing to act on a key is not a reason to delete it: a settings change in
// the app re-encodes the whole file.
func TestRestrictedKeysSurviveASave(t *testing.T) {
	cfg, tree := writeTreeConfig(t, execKnobs)
	if notes := cfg.RestrictIfInsideTree(tree); len(notes) == 0 {
		t.Fatal("control failed: nothing was restricted")
	}
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	back, err := Load(cfg.Path())
	if err != nil {
		t.Fatal(err)
	}
	if len(back.MCP) == 0 || len(back.LSP) == 0 || len(back.Agents.External) == 0 ||
		len(back.BashAllow) == 0 || back.PermissionMode != "bypass" {
		t.Error("saving deleted keys the user wrote; only this run should ignore them")
	}
}
