package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// touchExternally simulates the user editing a file in their own editor.
// mtime is bumped explicitly: a same-second write can otherwise be
// indistinguishable from ours on coarse-resolution filesystems.
func touchExternally(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	future := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(path, future, future); err != nil {
		t.Fatal(err)
	}
}

func allow(string, string) Decision { return Decision{Allow: true} }

func TestWriteRefusesToClobberExternalEdit(t *testing.T) {
	work := t.TempDir()
	path := filepath.Join(work, "notes.txt")
	if err := os.WriteFile(path, []byte("original\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	e := NewExecutor(work, ModeAuto)
	ctx := context.Background()

	if _, isErr, err := e.Call(ctx, "read_file", json.RawMessage(`{"path":"notes.txt"}`), allow); err != nil || isErr {
		t.Fatal("read failed")
	}
	touchExternally(t, path, "the user's own work\n")

	out, isErr, err := e.Call(ctx, "write_file",
		json.RawMessage(`{"path":"notes.txt","content":"agent version"}`), allow)
	if err != nil {
		t.Fatal(err)
	}
	if !isErr || !strings.Contains(out, "changed on disk") {
		t.Fatalf("stale write was allowed: %q", out)
	}
	if data, _ := os.ReadFile(path); string(data) != "the user's own work\n" {
		t.Fatalf("user's edit was clobbered: %q", data)
	}

	// Re-reading clears the block.
	if _, isErr, _ := e.Call(ctx, "read_file", json.RawMessage(`{"path":"notes.txt"}`), allow); isErr {
		t.Fatal("re-read failed")
	}
	if out, isErr, _ := e.Call(ctx, "write_file",
		json.RawMessage(`{"path":"notes.txt","content":"agent version"}`), allow); isErr {
		t.Fatalf("write after re-read still blocked: %q", out)
	}
}

func TestEditRefusesStaleFileAndUntrackedWritesPass(t *testing.T) {
	work := t.TempDir()
	path := filepath.Join(work, "code.go")
	if err := os.WriteFile(path, []byte("package x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	e := NewExecutor(work, ModeAuto)
	ctx := context.Background()

	if _, isErr, _ := e.Call(ctx, "read_file", json.RawMessage(`{"path":"code.go"}`), allow); isErr {
		t.Fatal("read failed")
	}
	touchExternally(t, path, "package x // user comment\n")

	out, isErr, err := e.Call(ctx, "edit_file",
		json.RawMessage(`{"path":"code.go","old_string":"package x","new_string":"package y"}`), allow)
	if err != nil {
		t.Fatal(err)
	}
	if !isErr || !strings.Contains(out, "changed on disk") {
		t.Fatalf("stale edit was allowed: %q", out)
	}

	// A file the agent never read is not stale — creating one must still work.
	if out, isErr, _ := e.Call(ctx, "write_file",
		json.RawMessage(`{"path":"brand_new.txt","content":"hello"}`), allow); isErr {
		t.Fatalf("writing an untracked file was blocked: %q", out)
	}
}

func TestChangedFilesReportsOnceAndIgnoresOurOwnWrites(t *testing.T) {
	work := t.TempDir()
	path := filepath.Join(work, "a.txt")
	if err := os.WriteFile(path, []byte("one\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	e := NewExecutor(work, ModeAuto)
	ctx := context.Background()

	if _, isErr, _ := e.Call(ctx, "read_file", json.RawMessage(`{"path":"a.txt"}`), allow); isErr {
		t.Fatal("read failed")
	}
	if got := e.ChangedFiles(); len(got) != 0 {
		t.Fatalf("untouched file reported as changed: %v", got)
	}

	// Our own edit must not be reported back to us as an external change.
	if _, isErr, _ := e.Call(ctx, "write_file",
		json.RawMessage(`{"path":"a.txt","content":"agent wrote this"}`), allow); isErr {
		t.Fatal("write failed")
	}
	if got := e.ChangedFiles(); len(got) != 0 {
		t.Fatalf("our own write reported as external: %v", got)
	}

	touchExternally(t, path, "user wrote this\n")
	got := e.ChangedFiles()
	if len(got) != 1 || got[0] != "a.txt" {
		t.Fatalf("external change not reported: %v", got)
	}
	if again := e.ChangedFiles(); len(again) != 0 {
		t.Fatalf("same change reported twice: %v", again)
	}
}
