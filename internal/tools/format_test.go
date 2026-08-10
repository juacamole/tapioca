package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestEditProducesADiff(t *testing.T) {
	work := t.TempDir()
	path := filepath.Join(work, "main.go")
	if err := os.WriteFile(path, []byte("package main\n\nfunc main() {\n\tx := 1\n}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	e := NewExecutor(work, ModeBypass)
	raw, _ := json.Marshal(map[string]any{
		"path": "main.go", "old_string": "x := 1", "new_string": "x := 2",
	})
	res, err := e.CallDetailed(context.Background(), "edit_file", raw, allow)
	if err != nil || res.IsErr {
		t.Fatalf("edit failed: %+v %v", res, err)
	}
	if res.Change == nil {
		t.Fatal("no diff produced for an edit")
	}
	if res.Change.Added != 1 || res.Change.Removed != 1 {
		t.Errorf("counts = +%d -%d, want +1 -1", res.Change.Added, res.Change.Removed)
	}
	if res.Change.Created {
		t.Error("editing an existing file marked as created")
	}
	out := FormatChange(res.Change)
	if !strings.Contains(out, "updated main.go (+1 -1)") {
		t.Errorf("header wrong:\n%s", out)
	}
	if !strings.Contains(out, "- \tx := 1") || !strings.Contains(out, "+ \tx := 2") {
		t.Errorf("changed lines missing:\n%s", out)
	}
	// The model gets the terse text; the diff is display-only.
	if strings.Contains(res.Text, "x := 2") {
		t.Errorf("diff leaked into the model-facing result: %q", res.Text)
	}
}

func TestWriteNewFileMarkedCreated(t *testing.T) {
	work := t.TempDir()
	e := NewExecutor(work, ModeBypass)
	raw, _ := json.Marshal(map[string]any{"path": "new.txt", "content": "hello\nworld\n"})
	res, err := e.CallDetailed(context.Background(), "write_file", raw, allow)
	if err != nil || res.IsErr {
		t.Fatalf("write failed: %+v %v", res, err)
	}
	if res.Change == nil || !res.Change.Created {
		t.Fatalf("new file not marked created: %+v", res.Change)
	}
	if res.Change.Added != 2 || res.Change.Removed != 0 {
		t.Errorf("counts = +%d -%d, want +2 -0", res.Change.Added, res.Change.Removed)
	}
	if !strings.Contains(FormatChange(res.Change), "created new.txt (+2 -0)") {
		t.Errorf("header wrong:\n%s", FormatChange(res.Change))
	}
}

func TestRewritingIdenticalContentHasNoDiff(t *testing.T) {
	work := t.TempDir()
	path := filepath.Join(work, "same.txt")
	os.WriteFile(path, []byte("unchanged\n"), 0o644)
	e := NewExecutor(work, ModeBypass)
	raw, _ := json.Marshal(map[string]any{"path": "same.txt", "content": "unchanged\n"})
	res, _ := e.CallDetailed(context.Background(), "write_file", raw, allow)
	if res.Change != nil {
		t.Errorf("a no-op write reported a change: %+v", res.Change)
	}
	if FormatChange(nil) != "" {
		t.Error("FormatChange(nil) should be empty")
	}
}
