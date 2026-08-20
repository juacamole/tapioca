package tools

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// deepMissing is a path with more components than realPath's link budget, so
// resolving it runs out and yields the unresolvable sentinel.
func deepMissing(root string) string {
	p := root
	for i := 0; i < 60; i++ {
		p = filepath.Join(p, "d")
	}
	return filepath.Join(p, "target")
}

// realPath answers unresolvable when it runs out of budget, and its comment
// says that value "never lies inside a work area". Nothing enforced it: a root
// that is itself unresolvable compares equal to the sentinel, so
// under(unresolvable, unresolvable) made every unreadable path read as inside
// the working directory. In auto mode that is a file write with no prompt at
// all — the one answer meaning "I could not tell" turned into "yes".
func TestUnresolvablePathIsNeverInsideTheWorkArea(t *testing.T) {
	cwd := t.TempDir()
	e := NewExecutor(cwd, ModeAuto)
	// An --add-dir the user typed that cannot be resolved: the control is that
	// the root really does exhaust the budget, or the test proves nothing.
	root := deepMissing(t.TempDir())
	if realPath(filepath.Clean(root)) != unresolvable {
		t.Skip("this platform resolves the root; the test cannot prove anything")
	}
	e.SetExtraDirs([]string{root})

	path := deepMissing(t.TempDir())
	if got := e.resolve(path); got != unresolvable {
		t.Fatalf("resolve(%s) = %q, want the unresolvable sentinel", path, got)
	}
	if e.inWorkArea(unresolvable) {
		t.Error("a path that could not be resolved was judged to be inside the working directory")
	}

	// What that decides: in auto mode an edit inside the work area does not
	// prompt, and one outside it does.
	asked := ""
	raw, _ := json.Marshal(map[string]string{"path": path, "content": "x"})
	if _, ok := e.Approve("write_file", raw, func(tool, summary string) Decision {
		asked = tool
		return Decision{Allow: true}
	}); !ok {
		t.Fatal("the write was refused outright, which is not what this is about")
	}
	if !strings.Contains(asked, "outside") {
		t.Errorf("auto mode approved a write to an unresolvable path without the outside-the-working-directory prompt (asked = %q)", asked)
	}
}

// The guard above must not make an ordinary tree unreadable: a working
// directory reached through a symlink is the case round 7 fixed, and it stays
// fixed.
func TestOrdinaryWorkAreaStillCountsAsInside(t *testing.T) {
	real := t.TempDir()
	link := filepath.Join(t.TempDir(), "work")
	if err := os.Symlink(real, link); err != nil {
		t.Skip("symlinks unavailable: " + err.Error())
	}
	if err := os.WriteFile(filepath.Join(real, "main.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	e := NewExecutor(link, ModeAuto)
	if !e.inWorkArea(e.resolve("main.go")) {
		t.Error("a file in the working directory read as outside it")
	}
	raw, _ := json.Marshal(map[string]string{"path": "main.go", "old_string": "package main", "new_string": "package x"})
	if _, ok := e.Approve("edit_file", raw, func(tool, summary string) Decision {
		t.Errorf("auto mode prompted for an ordinary in-tree edit: %s %s", tool, summary)
		return Decision{Allow: true}
	}); !ok {
		t.Error("an ordinary in-tree edit was refused")
	}
}
