package tools

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A bounded reading that fails open is a way through the bound: nest past the
// cap and the denied command is in none of the emitted forms.
func TestNestingPastTheCapIsStillDenied(t *testing.T) {
	for _, depth := range []int{1, 5, 16, 17, 25, 100} {
		e := NewExecutor(t.TempDir(), "bypass")
		e.SetRules(nil, nil, []string{"bash(touch*)"})
		cmd := "echo " + strings.Repeat("$(", depth) + "touch M" + strings.Repeat(")", depth)
		act := e.ruleFor("bash", cmd)
		t.Logf("nesting=%-4d rule=%q", depth, act)
		if act != RuleDeny {
			t.Errorf("depth %d: touch was not denied", depth)
		}
	}
}

// Every path is resolved through realPath, so comparing the result against an
// unresolved cwd can never match when the working directory is reached through
// a symlink -- /tmp on macOS, ~/src -> /data/src, a NixOS or corporate home.
func TestSymlinkedWorkingDirectoryIsNotOutsideItself(t *testing.T) {
	real := t.TempDir()
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(real, link); err != nil {
		t.Skip("symlinks unavailable")
	}
	if err := os.WriteFile(filepath.Join(real, "main.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	e := NewExecutor(link, "auto")
	inside := filepath.Join(link, "main.go")
	if !e.inWorkArea(e.resolve(inside)) {
		t.Errorf("a file in the working directory read as outside it: %s", inside)
	}
	// Control: something genuinely outside must still be outside.
	if e.inWorkArea(e.resolve(filepath.Join(t.TempDir(), "elsewhere.txt"))) {
		t.Error("a path outside the working directory read as inside")
	}
}
