package tools

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// git stores symlinks, so a cloned repository can ship one pointing anywhere.
// Every boundary check compares resolved paths as strings, so if resolve stops
// at the link the checks are looking at a path the write never touches.
func TestSymlinkCannotEscapeTheWorkArea(t *testing.T) {
	e := execIn(t, ModeAuto)
	outside := t.TempDir()
	if real, err := filepath.EvalSymlinks(outside); err == nil {
		outside = real
	}
	victim := filepath.Join(outside, "authorized_keys")
	if err := os.WriteFile(victim, []byte("original\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(e.Cwd(), "docs")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	var asked []string
	res, err := e.CallDetailed(context.Background(), "write_file",
		args(t, map[string]string{"path": "docs/authorized_keys", "content": "attacker"}),
		asker(Decision{Allow: false}, &asked))
	if err != nil {
		t.Fatal(err)
	}
	if len(asked) == 0 {
		t.Error("auto mode wrote through a symlink out of the worktree without asking")
	}
	if !res.IsErr {
		t.Error("the denial did not stop the write")
	}
	if data, _ := os.ReadFile(victim); string(data) != "original\n" {
		t.Fatalf("file outside the work area was overwritten: %q", data)
	}
}

// The read gate keys off the same resolution.
func TestSymlinkCannotHideASensitivePath(t *testing.T) {
	e := execIn(t, ModeManual)
	home := t.TempDir()
	if real, err := filepath.EvalSymlinks(home); err == nil {
		home = real
	}
	t.Setenv("HOME", home)
	ssh := filepath.Join(home, ".ssh")
	if err := os.MkdirAll(ssh, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ssh, "id_ed25519"), []byte("PRIVATE-CANARY"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(ssh, filepath.Join(e.Cwd(), "vendor")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if !e.sensitivePath("vendor/id_ed25519") {
		t.Error("a symlinked path into ~/.ssh did not read as sensitive")
	}
	var asked []string
	out, isErr, err := e.Call(context.Background(), "read_file",
		args(t, map[string]string{"path": "vendor/id_ed25519"}), asker(Decision{Allow: false}, &asked))
	if err != nil {
		t.Fatal(err)
	}
	if len(asked) == 0 || !isErr {
		t.Fatalf("key read without a prompt: %q", out)
	}
}

// Writing a file that does not exist yet still has to resolve the directories
// leading to it, or the fix only covers overwrites.
func TestNewFileThroughASymlinkedDirIsResolved(t *testing.T) {
	e := execIn(t, ModeManual)
	outside := t.TempDir()
	if real, err := filepath.EvalSymlinks(outside); err == nil {
		outside = real
	}
	if err := os.Symlink(outside, filepath.Join(e.Cwd(), "link")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	got := e.resolve("link/new/file.txt")
	want := filepath.Join(outside, "new", "file.txt")
	if got != want {
		t.Fatalf("resolve = %q, want %q", got, want)
	}
	if e.inWorkArea(got) {
		t.Error("a path outside the worktree still read as inside")
	}
}

// Ordinary paths must survive the change untouched.
func TestResolveLeavesOrdinaryPathsAlone(t *testing.T) {
	e := execIn(t, ModeManual)
	if got, want := e.resolve("a/b.go"), filepath.Join(e.Cwd(), "a", "b.go"); got != want {
		t.Fatalf("resolve = %q, want %q", got, want)
	}
	if got, want := e.resolve("./x/../a/b.go"), filepath.Join(e.Cwd(), "a", "b.go"); got != want {
		t.Fatalf("resolve = %q, want %q", got, want)
	}
}
