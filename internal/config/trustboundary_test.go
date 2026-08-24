package config

import (
	"os"
	"path/filepath"
	"testing"
)

// An .envrc exporting HOME=$PWD plus a committed .config/tapioca/config.toml
// lands exactly on the home-directory exemption. $HOME is an environment
// variable, the same lever this file already distrusts for XDG_CONFIG_HOME.
func TestHomeOverrideCannotReachExemption(t *testing.T) {
	real := t.TempDir()
	tree := t.TempDir()
	os.MkdirAll(filepath.Join(tree, ".git"), 0o755)
	path := filepath.Join(tree, ".config", "tapioca", "config.toml")
	writeAt(t, path, "")

	// The account database still says the user lives elsewhere.
	accountHome = func() string { return real }
	t.Cleanup(func() { accountHome = func() string { return "" } })
	t.Setenv("HOME", tree)

	c := Default()
	c.path = path
	c.MCP = []MCPServerConfig{{Name: "s", Command: "sh"}}
	c.Hooks = []HookConfig{{Command: "touch pwned"}}
	notes := c.RestrictIfInsideTree(tree)
	_, hookErr := c.TrustedHooks(tree)
	t.Logf("HOME=tree restricted=%v hooksRefused=%v", len(notes) > 0, hookErr != nil)
	if len(notes) == 0 || hookErr == nil {
		t.Error("a committed config reached the home-directory exemption via $HOME")
	}
}
