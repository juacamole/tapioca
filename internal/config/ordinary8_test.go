package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Only ordinary setups here: a config the user wrote, in a place the user
// chose, with values the user meant. Nothing in this file is a hostile tree.

// fullConfig is a config with one of every key the in-tree restriction drops,
// so a failure names which of them stopped working.
func fullConfig(t *testing.T, path string) *Config {
	t.Helper()
	c := Default()
	c.path = path
	c.Hooks = []HookConfig{{Event: "session_start", Command: "true"}}
	c.MCP = []MCPServerConfig{{Name: "fs", Command: "mcp-server-filesystem"}}
	c.LSP = []LSPServerConfig{{Name: "gopls", Command: "gopls"}}
	c.BashAllow = []string{"git", "go"}
	c.Permissions.Allow = []string{"bash(go test*)"}
	c.PermissionMode = "auto"
	c.Editor = "nvim"
	return c
}

func assertNothingDropped(t *testing.T, c *Config, cwd, what string) {
	t.Helper()
	if notes := c.RestrictIfInsideTree(cwd); len(notes) > 0 {
		t.Errorf("%s: the user's own config was restricted: %v", what, notes)
	}
	if _, err := c.TrustedHooks(cwd); err != nil {
		t.Errorf("%s: the user's own hooks were refused: %v", what, err)
	}
	if len(c.MCP) == 0 {
		t.Errorf("%s: mcp servers were dropped", what)
	}
	if len(c.LSP) == 0 {
		t.Errorf("%s: language servers were dropped", what)
	}
	if len(c.BashAllow) == 0 {
		t.Errorf("%s: bash_allow was dropped", what)
	}
	if len(c.Permissions.Allow) == 0 {
		t.Errorf("%s: permissions.allow was dropped", what)
	}
	if c.PermissionMode != "auto" {
		t.Errorf("%s: permission_mode was reset to %q", what, c.PermissionMode)
	}
	if c.Editor == "" {
		t.Errorf("%s: editor was dropped", what)
	}
}

// A dotfiles repository checked out in $HOME is an ordinary setup, and it puts
// ~/.config/tapioca/config.toml literally inside a git tree. The config is
// still the user's own.
func TestOrdinaryDotfilesRepoInHomeKeepsTheUsersOwnConfig(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("TAPIOCA_CONFIG_DIR", "")
	pretendHome(t, home)
	if err := os.MkdirAll(filepath.Join(home, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(home, ".config", "tapioca", "config.toml")
	writeAt(t, path, "")

	assertNothingDropped(t, fullConfig(t, path), home, "dotfiles repo in $HOME")
}

// The same, worked on from a subdirectory of that dotfiles repo — editing
// ~/.config/nvim while the repo root is $HOME.
func TestOrdinaryDotfilesRepoWorkedOnFromASubdirectory(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("TAPIOCA_CONFIG_DIR", "")
	pretendHome(t, home)
	if err := os.MkdirAll(filepath.Join(home, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	sub := filepath.Join(home, ".config", "nvim")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(home, ".config", "tapioca", "config.toml")
	writeAt(t, path, "")

	assertNothingDropped(t, fullConfig(t, path), sub, "subdirectory of a dotfiles repo")
}

// A container with no passwd entry for the uid is ordinary. There is then no
// second opinion about where home is, and the user's own config must not become
// untrusted for want of one.
func TestOrdinaryContainerWithNoPasswdEntry(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("TAPIOCA_CONFIG_DIR", "")
	old := accountHome
	accountHome = func() string { return "" }
	t.Cleanup(func() { accountHome = old })

	if err := os.MkdirAll(filepath.Join(home, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(home, ".config", "tapioca", "config.toml")
	writeAt(t, path, "")

	assertNothingDropped(t, fullConfig(t, path), home, "container with no passwd entry")
}

// XDG_CONFIG_HOME pointed somewhere normal and outside every tree — the usual
// reason people set it at all.
func TestOrdinaryXDGConfigHomeOutsideTheTree(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	pretendHome(t, home)
	xdg := filepath.Join(t.TempDir(), "xdgconfig")
	t.Setenv("XDG_CONFIG_HOME", xdg)
	t.Setenv("TAPIOCA_CONFIG_DIR", "")
	path := filepath.Join(xdg, "tapioca", "config.toml")
	writeAt(t, path, "")

	project := t.TempDir()
	if err := os.MkdirAll(filepath.Join(project, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	assertNothingDropped(t, fullConfig(t, path), project, "XDG_CONFIG_HOME outside the tree")
}

// TAPIOCA_CONFIG_DIR pointed at a directory in $HOME that is not ~/.config,
// with a dotfiles repo in $HOME, is restricted: the file is inside the tree
// being worked on, and the exemption knows exactly one path — the default one,
// which an archive cannot occupy without also redirecting HOME, and which
// usersHome() cross-checks against the account database.
//
// This is a real cost to a real setup and it is pinned rather than widened,
// because nothing in the process can tell it apart from the attack. "The
// config dir is under $HOME" does not do it: a checkout is under $HOME too, so
// an .envrc setting TAPIOCA_CONFIG_DIR=$PWD/.tapioca in a repository cloned
// into $HOME/src lands on exactly the same test. What the user can do instead
// is in the message, so the assertion is that the message says it.
func TestOrdinaryTapiocaConfigDirInAHomeDotfilesRepoIsRestricted(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	pretendHome(t, home)
	t.Setenv("XDG_CONFIG_HOME", "")
	dir := filepath.Join(home, ".tapioca")
	t.Setenv("TAPIOCA_CONFIG_DIR", dir)
	if err := os.MkdirAll(filepath.Join(home, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "config.toml")
	writeAt(t, path, "")

	cfg := fullConfig(t, path)
	notes := cfg.RestrictIfInsideTree(home)
	if len(notes) == 0 {
		t.Fatal("a config dir named by an environment variable inside the tree was trusted")
	}
	for _, n := range notes {
		if !strings.Contains(n, "TAPIOCA_CONFIG_DIR") {
			t.Errorf("the refusal does not say what to do about it: %s", n)
		}
	}
	// The default spelling of the same directory is the one that is exempt,
	// and it has to stay exempt or a dotfiles repo is unusable.
	t.Setenv("TAPIOCA_CONFIG_DIR", filepath.Join(home, ".config", "tapioca"))
	def := filepath.Join(home, ".config", "tapioca", "config.toml")
	writeAt(t, def, "")
	assertNothingDropped(t, fullConfig(t, def), home, "the default config path in a $HOME dotfiles repo")
}

// A LAN or localhost model server named in the user's own, out-of-tree config
// is the ordinary way to use ollama or llama.cpp.
func TestOrdinaryLocalModelServerBaseURLSurvives(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	pretendHome(t, home)
	xdg := filepath.Join(t.TempDir(), "xdgconfig")
	t.Setenv("XDG_CONFIG_HOME", xdg)
	t.Setenv("TAPIOCA_CONFIG_DIR", "")
	path := filepath.Join(xdg, "tapioca", "config.toml")
	writeAt(t, path, "")

	project := t.TempDir()
	if err := os.MkdirAll(filepath.Join(project, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, base := range []string{
		"http://192.168.1.50:11434",
		"http://127.0.0.1:11434",
		"http://localhost:8080",
		"https://api.openai.com/v1",
	} {
		c := Default()
		c.path = path
		c.Providers = map[string]ProviderConfig{"local": {Type: "ollama", BaseURL: base}}
		if notes := c.RestrictIfInsideTree(project); len(notes) > 0 {
			t.Errorf("base_url %q in the user's own config was restricted: %v", base, notes)
		}
		if got := c.Providers["local"].BaseURL; got != base {
			t.Errorf("base_url %q was rewritten to %q", base, got)
		}
	}
}

// A real cloud region and a real vertex project in the user's own out-of-tree
// config must survive untouched.
func TestOrdinaryRegionAndProjectSurvive(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	pretendHome(t, home)
	xdg := filepath.Join(t.TempDir(), "xdgconfig")
	t.Setenv("XDG_CONFIG_HOME", xdg)
	t.Setenv("TAPIOCA_CONFIG_DIR", "")
	path := filepath.Join(xdg, "tapioca", "config.toml")
	writeAt(t, path, "")

	project := t.TempDir()
	if err := os.MkdirAll(filepath.Join(project, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	c := Default()
	c.path = path
	c.Providers = map[string]ProviderConfig{
		"bedrock": {Type: "bedrock", Region: "us-east-1"},
		"eu":      {Type: "bedrock", Region: "eu-central-1"},
		"vertex":  {Type: "vertex", Region: "europe-west4", Project: "my-gcp-project-123"},
	}
	if notes := c.RestrictIfInsideTree(project); len(notes) > 0 {
		t.Errorf("regions in the user's own config were restricted: %v", notes)
	}
	for name, want := range map[string]string{"bedrock": "us-east-1", "eu": "eu-central-1", "vertex": "europe-west4"} {
		if got := c.Providers[name].Region; got != want {
			t.Errorf("region for %s became %q, want %q", name, got, want)
		}
	}
	if got := c.Providers["vertex"].Project; got != "my-gcp-project-123" {
		t.Errorf("vertex project became %q", got)
	}
}

// A localhost base_url is expressly meant to survive even the in-tree
// restriction, since a local model server is the one reason a checkout has to
// name an address at all.
func TestOrdinaryLoopbackHostsAreRecognised(t *testing.T) {
	for _, h := range []string{"localhost", "localhost.", "127.0.0.1", "::1", "[::1]", "127.5.5.5"} {
		if !IsLoopbackHost(h) {
			t.Errorf("%q is not recognised as this machine", h)
		}
	}
}
