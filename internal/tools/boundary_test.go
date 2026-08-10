package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// asks records every prompt and answers with the given decision.
func asks(d Decision, log *[]string) AskFunc {
	return func(tool, summary string) Decision {
		*log = append(*log, tool)
		return d
	}
}

func writeCall(t *testing.T, e *Executor, path, content string, ask AskFunc) Result {
	t.Helper()
	raw, _ := json.Marshal(map[string]string{"path": path, "content": content})
	res, err := e.CallDetailed(context.Background(), "write_file", raw, ask)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

// Auto mode auto-approves edits, but "edits" means the project — not ~/.zshrc
// or Tapioca's own config, which seeds bash_allow on the next start.
func TestAutoModeStillAsksForWritesOutsideTheWorktree(t *testing.T) {
	work := t.TempDir()
	outside := t.TempDir()
	e := NewExecutor(work, ModeAuto)

	var log []string
	if res := writeCall(t, e, "inside.txt", "ok", asks(Decision{Allow: true}, &log)); res.IsErr {
		t.Fatalf("in-tree write failed: %s", res.Text)
	}
	if len(log) != 0 {
		t.Errorf("auto mode prompted for an in-tree write: %v", log)
	}

	target := filepath.Join(outside, "victim.txt")
	log = nil
	res := writeCall(t, e, target, "pwned", asks(Decision{}, &log))
	if !res.IsErr {
		t.Error("auto mode wrote outside the worktree without asking")
	}
	if len(log) != 1 || !strings.Contains(log[0], "outside the working directory") {
		t.Errorf("prompt did not flag the location: %v", log)
	}
	if _, err := os.Stat(target); err == nil {
		t.Error("the denied file was written anyway")
	}
}

// "Always allow" on an outside write must not become a licence to write
// anywhere for the rest of the session.
func TestAlwaysOnAnOutsideWriteGrantsOnlyThatPath(t *testing.T) {
	work := t.TempDir()
	outside := t.TempDir()
	e := NewExecutor(work, ModeAuto)
	first := filepath.Join(outside, "one.txt")
	second := filepath.Join(outside, "two.txt")

	var log []string
	if res := writeCall(t, e, first, "a", asks(Decision{Allow: true, Always: true}, &log)); res.IsErr {
		t.Fatalf("granted write failed: %s", res.Text)
	}
	log = nil
	if res := writeCall(t, e, first, "b", asks(Decision{}, &log)); res.IsErr {
		t.Error("the granted path asked again")
	}
	if len(log) != 0 {
		t.Errorf("granted path prompted: %v", log)
	}

	log = nil
	if res := writeCall(t, e, second, "c", asks(Decision{}, &log)); !res.IsErr {
		t.Error("the grant leaked to another path outside the worktree")
	}
	if len(log) != 1 {
		t.Errorf("expected one prompt for the new path, got %v", log)
	}
}

// A blanket "always allow write_file" is answered about the project; it must
// not silently extend beyond it.
func TestBlanketWriteGrantDoesNotCoverOutside(t *testing.T) {
	work := t.TempDir()
	outside := t.TempDir()
	e := NewExecutor(work, ModeManual)

	var log []string
	writeCall(t, e, "in.txt", "a", asks(Decision{Allow: true, Always: true}, &log))
	log = nil
	if res := writeCall(t, e, "in2.txt", "b", asks(Decision{}, &log)); res.IsErr {
		t.Error("blanket grant did not cover a second in-tree write")
	}
	log = nil
	if res := writeCall(t, e, filepath.Join(outside, "x.txt"), "c", asks(Decision{}, &log)); !res.IsErr {
		t.Error("blanket write_file grant reached outside the worktree")
	}
}

func TestEditOutsideWorktreeAsksInAutoMode(t *testing.T) {
	work := t.TempDir()
	outside := t.TempDir()
	target := filepath.Join(outside, "conf.txt")
	if err := os.WriteFile(target, []byte("original\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	e := NewExecutor(work, ModeAuto)

	var log []string
	raw, _ := json.Marshal(map[string]any{
		"path": target, "old_string": "original", "new_string": "tampered",
	})
	res, err := e.CallDetailed(context.Background(), "edit_file", raw, asks(Decision{}, &log))
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsErr || len(log) != 1 {
		t.Fatalf("edit outside the worktree was auto-approved: %+v %v", res, log)
	}
	if data, _ := os.ReadFile(target); strings.Contains(string(data), "tampered") {
		t.Error("the file was edited despite the denial")
	}
}

// --add-dir trees are announced as work areas, so they behave like the tree.
func TestExtraDirsCountAsInsideForWrites(t *testing.T) {
	work := t.TempDir()
	extra := t.TempDir()
	e := NewExecutor(work, ModeAuto)
	e.SetExtraDirs([]string{extra})

	var log []string
	if res := writeCall(t, e, filepath.Join(extra, "f.txt"), "x", asks(Decision{}, &log)); res.IsErr {
		t.Errorf("write into an --add-dir tree was blocked: %s", res.Text)
	}
	if len(log) != 0 {
		t.Errorf("--add-dir tree prompted: %v", log)
	}
}
