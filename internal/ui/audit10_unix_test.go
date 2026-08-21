//go:build unix

package ui

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"tapioca/internal/agent"
	"tapioca/internal/config"
	"tapioca/internal/tools"
)

// fifoBlocksHere reports whether opening a FIFO for reading actually blocks on
// this machine. A test that asserts "the loader did not hang" proves nothing
// where the open returns straight away.
func fifoBlocksHere(t *testing.T, path string) bool {
	t.Helper()
	if err := syscall.Mkfifo(path, 0o644); err != nil {
		t.Skipf("cannot create a fifo here: %v", err)
	}
	opened := make(chan struct{})
	go func() {
		if f, err := os.Open(path); err == nil {
			f.Close()
		}
		close(opened)
	}()
	select {
	case <-opened:
		return false
	case <-time.After(500 * time.Millisecond):
		return true
	}
}

// .tapioca/commands is read from the working tree, at startup, before the user
// has typed anything — and a file in an extracted tarball need not be a file.
// project.readCapped and skills.readCapped both decide the kind of file before
// opening it for exactly this reason; the command loader opened whatever was
// there, so an archive shipping .tapioca/commands/review.md as a FIFO wedged
// the app on the way up, with nothing to cancel and no deadline on the open.
func TestUserCommandFifoDoesNotWedgeStartup(t *testing.T) {
	t.Setenv("TAPIOCA_CONFIG_DIR", t.TempDir())
	work := t.TempDir()
	dir := filepath.Join(work, ".tapioca", "commands")

	// The control: an ordinary command in the same directory is loaded, so a
	// later "it returned and the fifo was skipped" is about the fifo and not
	// about the loader never looking here.
	writeCmd(t, dir, "zzship.md", "# Ship it\nRun the tests.")
	if got := loadUserCmds(work); len(got) != 1 || got[0].name != "zzship" {
		t.Fatalf("the loader does not read %s: %+v", dir, got)
	}

	// Named to sort before the real command, so the fifo is reached first.
	if !fifoBlocksHere(t, filepath.Join(dir, "aareview.md")) {
		t.Skip("opening a fifo does not block here")
	}

	done := make(chan []userCmd, 1)
	go func() { done <- loadUserCmds(work) }()
	select {
	case got := <-done:
		if len(got) != 1 || got[0].name != "zzship" {
			t.Errorf("loaded %+v, want only the ordinary command", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("loadUserCmds hung on .tapioca/commands/aareview.md being a fifo")
	}
}

// An @mention inlines a file into the outgoing prompt, and it does it on the
// update goroutine — so a read that never returns is the whole TUI stopping,
// not one slow message. The size cap in front of it bounds what a read costs
// and cannot bound one that never starts: a FIFO reports a size of zero.
// mention completion offers every file in the tree, a repository chooses their
// names, and tar extracts FIFOs.
func TestAMentionedFifoDoesNotWedgeTheUpdateLoop(t *testing.T) {
	t.Setenv("TAPIOCA_CONFIG_DIR", t.TempDir())
	work := t.TempDir()

	exec := tools.NewExecutor(work, tools.ModeManual)
	m := &App{cfg: config.Default(), mgr: agent.NewManager(config.Default(), nil, exec), w: 100, h: 30}

	// The control: an ordinary file in the same tree really is inlined, so a
	// later "the fifo was left out" is about the fifo.
	if err := os.WriteFile(filepath.Join(work, "ok.md"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	var atts []attachment
	if got := m.expandMentions("look at @ok.md", &atts); !strings.Contains(got, "hello") {
		t.Fatalf("an ordinary mention is not inlined: %q", got)
	}

	if !fifoBlocksHere(t, filepath.Join(work, "notes.md")) {
		t.Skip("opening a fifo does not block here")
	}
	done := make(chan string, 1)
	go func() {
		var atts []attachment
		done <- m.expandMentions("look at @notes.md", &atts)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("expandMentions hung on a mention of a fifo in the working tree")
	}
}
