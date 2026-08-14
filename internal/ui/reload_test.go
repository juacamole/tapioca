package ui

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"tapioca/internal/config"
)

func reloadApp(t *testing.T) (*App, string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(path, []byte("max_tokens = 4096\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	m := dashApp(t, 100, 30)
	m.cfg = cfg
	m.noteReloadStamps()
	return m, path
}

// The gap: an edit made outside the app did nothing until /settings was opened
// and quit out of again.
func TestAnExternalEditIsPickedUp(t *testing.T) {
	m, path := reloadApp(t)
	if m.cfg.MaxTokens != 4096 {
		t.Fatalf("started at %d", m.cfg.MaxTokens)
	}

	if err := os.WriteFile(path, []byte("max_tokens = 32000\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	m.checkReload()

	if m.cfg.MaxTokens != 32000 {
		t.Errorf("max_tokens is %d after an external edit, want 32000", m.cfg.MaxTokens)
	}
}

// Nothing changed means nothing happens — a poll that reloaded every tick
// would rebuild providers and reset agents continuously.
func TestNoChangeDoesNotReload(t *testing.T) {
	m, _ := reloadApp(t)
	m.cfg.MaxTokens = 999 // a value only an in-memory change would have

	m.checkReload()

	if m.cfg.MaxTokens != 999 {
		t.Error("an unchanged file triggered a reload")
	}
}

// Editors write partial files constantly, and with a watcher that is no longer
// rare: a half-saved config must not take a running session down.
func TestABrokenConfigLeavesThePreviousOneRunning(t *testing.T) {
	m, path := reloadApp(t)
	m.cfg.MaxTokens = 4096

	if err := os.WriteFile(path, []byte("max_tokens = = = broken\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	m.checkReload()

	if m.cfg.MaxTokens != 4096 {
		t.Errorf("a broken config changed the running one to %d", m.cfg.MaxTokens)
	}
	if !strings.Contains(m.flash, "still running") {
		t.Errorf("the error does not say the old config survives: %q", m.flash)
	}
}

// A file that will not parse must be reported once, not retried every tick —
// otherwise a broken config floods the status line until it is fixed.
func TestABrokenConfigIsNotRetriedEveryTick(t *testing.T) {
	m, path := reloadApp(t)
	if err := os.WriteFile(path, []byte("broken = = =\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	m.checkReload()
	m.flash = ""
	m.checkReload() // same broken file, unchanged

	if m.flash != "" {
		t.Errorf("the same broken file reported again: %q", m.flash)
	}
}

// A reload we performed must not then look like an external change and be
// performed again.
func TestOurOwnReloadIsNotSeenAsAChange(t *testing.T) {
	m, path := reloadApp(t)
	if err := os.WriteFile(path, []byte("max_tokens = 8192\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	m.checkReload()
	m.cfg.MaxTokens = 1234 // stand in for a later in-memory change

	m.checkReload()

	if m.cfg.MaxTokens != 1234 {
		t.Error("the reload was applied a second time")
	}
}

// /reload does the same thing without an editor.
func TestReloadCommandRereadsTheFile(t *testing.T) {
	m, path := reloadApp(t)
	if err := os.WriteFile(path, []byte("max_tokens = 16384\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cmdReload(m, "")
	if m.cfg.MaxTokens != 16384 {
		t.Errorf("max_tokens is %d after /reload, want 16384", m.cfg.MaxTokens)
	}
}

// A command file appearing on disk has to count as a change, since "the agent
// wrote a command and cannot use it" is the case that motivated this.
func TestANewCommandFileIsNoticed(t *testing.T) {
	m, _ := reloadApp(t)
	dirs := m.commandDirs()
	if len(dirs) == 0 {
		t.Skip("no command directories")
	}
	dir := dirs[len(dirs)-1] // the project-local one, under the temp cwd
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Skip("cannot create the command directory here")
	}
	t.Cleanup(func() { os.RemoveAll(dir) })

	before := m.reloadStamp()
	if err := os.WriteFile(filepath.Join(dir, "newcmd.md"), []byte("do a thing\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if m.reloadStamp() == before {
		t.Error("a new command file did not change the fingerprint")
	}
}
