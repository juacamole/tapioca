package project

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// git stores symlinks, so a clone can ship AGENTS.md as a link to the user's
// config file. The system prompt is built on every turn, in every mode
// including plan, with no tool call anyone could decline.
func TestNoInstructionPathReachesTheConfigFile(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	cfgDir := filepath.Join(home, ".config", "tapioca")
	os.MkdirAll(cfgDir, 0o755)
	cfgPath := filepath.Join(cfgDir, "config.toml")
	os.WriteFile(cfgPath, []byte("[providers.anthropic]\napi_key = \"sk-CANARY\"\n"), 0o600)

	// A clone: a git repo whose AGENTS.md is a symlink to that config.
	tree := filepath.Join(home, "clone")
	os.MkdirAll(filepath.Join(tree, ".git"), 0o755)
	if err := os.Symlink(cfgPath, filepath.Join(tree, "AGENTS.md")); err != nil {
		t.Skip("symlinks unavailable")
	}
	if got := Instructions(tree); strings.Contains(got, "sk-CANARY") {
		t.Errorf("the config file reached the system prompt:\n%s", got)
	}

	// And the @import spelling, with a markdown-looking name.
	os.Remove(filepath.Join(tree, "AGENTS.md"))
	os.WriteFile(filepath.Join(tree, "AGENTS.md"), []byte("@keys.md\n"), 0o644)
	os.Symlink(cfgPath, filepath.Join(tree, "keys.md"))
	if got := Instructions(tree); strings.Contains(got, "sk-CANARY") {
		t.Errorf("an imported symlink reached the config file:\n%s", got)
	}
}
