package gitcmd

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// An include naming a file that is not there yet contributes no keys, so
// --show-origin never names it and the cache never learns to watch it. The
// file appearing later is an ordinary in-tree write — auto mode approves one
// without asking — and everything in it is live from that moment while the
// cache still answers with the pins read before it existed.
func TestPinsNoticeAKeyAddedToAnIncludeThatDidNotExistYet(t *testing.T) {
	dir, marker := setupFilterRepo(t, func(dir, hook string) {
		// lint.cfg is deliberately absent: git ignores a missing include in
		// silence, and a file that produced no key is a file no origin names.
		if out, err := exec.Command("git", "-C", dir, "config", "include.path", "../lint.cfg").CombinedOutput(); err != nil {
			t.Fatalf("%v %s", err, out)
		}
	})
	stale := filepath.Join(dir, "f.txt")
	dirty := func() {
		if err := os.Chtimes(stale, time.Unix(1, 0), time.Unix(1, 0)); err != nil {
			t.Fatal(err)
		}
	}
	// Opening the directory: the git panel polls status, and this repository's
	// pins are read and cached while the include points at nothing.
	dirty()
	_ = In(dir, "status", "--porcelain", "-b").Run()

	hook := filepath.Join(dir, "pwn.sh")
	if err := os.WriteFile(filepath.Join(dir, "lint.cfg"), []byte("[filter \"ev\"]\n\tclean = "+hook+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	dirty()
	if !runs(t, dir, marker, func() {
		_ = exec.Command("git", "-C", dir, "status", "--porcelain", "-b").Run()
	}) {
		t.Skip("this git does not run the filter here")
	}
	dirty()
	if runs(t, dir, marker, func() { _ = In(dir, "status", "--porcelain", "-b").Run() }) {
		t.Error("a filter written into an include that did not exist when the pins were cached still executed")
	}
}

// Not caching is the answer to "I cannot know which files git reads", and it
// must stay exactly that: the pins themselves are still read, so a repository
// that uses an include is not thereby left unpinned. Nothing here is a matter
// of taste — returning no pins is the hole this whole file exists to prevent.
func TestAnIncludeCostsTheCacheAndNotThePins(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	dir := t.TempDir()
	for _, args := range [][]string{
		{"init", "-q", "."},
		{"config", "core.fsmonitor", "/bin/true"},
		{"config", "include.path", "../lint.cfg"},
	} {
		if out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("setup %v: %v %s", args, err, out)
		}
	}
	repoPins(dir)
	pinMu.Lock()
	c := pinsByRepo[dir]
	pinMu.Unlock()
	if len(c.files) != 0 {
		t.Errorf("an include left files recorded, so the pins would be reused: %v", c.files)
	}
	if current(c.files) {
		t.Error("the cache reported itself current although an include may name a file it has never seen")
	}
	found := false
	for _, p := range c.pins {
		if strings.EqualFold(p.key, "core.fsmonitor") {
			found = true
		}
	}
	if !found {
		t.Errorf("the repository's own keys were not pinned: %v", c.pins)
	}
}

// A conditional include is reported as a key while its condition is false, and
// the file it names contributes nothing until the condition turns true. An
// onbranch condition turns true on a checkout, which writes .git/HEAD and
// leaves every file the pins were read from exactly as it was.
func TestPinsNoticeAnIncludeThatABranchSwitchTurnsOn(t *testing.T) {
	dir, marker := setupFilterRepo(t, func(dir, hook string) {
		if err := os.WriteFile(filepath.Join(dir, "lint.cfg"), []byte("[filter \"ev\"]\n\tclean = "+hook+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if out, err := exec.Command("git", "-C", dir, "config",
			"includeIf.onbranch:evil.path", "../lint.cfg").CombinedOutput(); err != nil {
			t.Fatalf("%v %s", err, out)
		}
	})
	stale := filepath.Join(dir, "f.txt")
	dirty := func() {
		if err := os.Chtimes(stale, time.Unix(1, 0), time.Unix(1, 0)); err != nil {
			t.Fatal(err)
		}
	}
	dirty()
	_ = In(dir, "status", "--porcelain", "-b").Run()

	if out, err := exec.Command("git", "-C", dir, "checkout", "-q", "-b", "evil").CombinedOutput(); err != nil {
		t.Skipf("cannot switch branch: %v %s", err, out)
	}
	dirty()
	if !runs(t, dir, marker, func() {
		_ = exec.Command("git", "-C", dir, "status", "--porcelain", "-b").Run()
	}) {
		t.Skip("this git does not honour the conditional include here")
	}
	dirty()
	if runs(t, dir, marker, func() { _ = In(dir, "status", "--porcelain", "-b").Run() }) {
		t.Error("a conditional include a checkout turned on still executed")
	}
}

// The same hole with the file present from the start: a config file holding
// nothing but a comment produces no key either, so it is named by no origin
// and watched by nothing. Shipping an empty build.cfg is less conspicuous than
// shipping an include that dangles.
func TestPinsNoticeAKeyAddedToAnIncludeThatWasEmpty(t *testing.T) {
	dir, marker := setupFilterRepo(t, func(dir, hook string) {
		if err := os.WriteFile(filepath.Join(dir, "lint.cfg"), []byte("# lint settings\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if out, err := exec.Command("git", "-C", dir, "config", "include.path", "../lint.cfg").CombinedOutput(); err != nil {
			t.Fatalf("%v %s", err, out)
		}
	})
	stale := filepath.Join(dir, "f.txt")
	dirty := func() {
		if err := os.Chtimes(stale, time.Unix(1, 0), time.Unix(1, 0)); err != nil {
			t.Fatal(err)
		}
	}
	dirty()
	_ = In(dir, "status", "--porcelain", "-b").Run()

	hook := filepath.Join(dir, "pwn.sh")
	if err := os.WriteFile(filepath.Join(dir, "lint.cfg"), []byte("[filter \"ev\"]\n\tclean = "+hook+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	dirty()
	if !runs(t, dir, marker, func() {
		_ = exec.Command("git", "-C", dir, "status", "--porcelain", "-b").Run()
	}) {
		t.Skip("this git does not run the filter here")
	}
	dirty()
	if runs(t, dir, marker, func() { _ = In(dir, "status", "--porcelain", "-b").Run() }) {
		t.Error("a filter written into an include that held no keys still executed")
	}
}
