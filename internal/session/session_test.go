package session

import (
	"os"
	"path/filepath"
	"testing"
)

// Enough ids that a purely probabilistic suffix collides here every time:
// a duplicate means one session file would overwrite another.
func TestNewIDUnique(t *testing.T) {
	t.Setenv("TAPIOCA_DATA_DIR", t.TempDir())
	seen := map[string]bool{}
	for i := 0; i < 5000; i++ {
		id := NewID()
		if seen[id] {
			t.Fatalf("duplicate id %s after %d ids", id, i)
		}
		seen[id] = true
	}
}

func TestNewIDSkipsIDsAlreadyOnDisk(t *testing.T) {
	t.Setenv("TAPIOCA_DATA_DIR", t.TempDir())
	if err := os.MkdirAll(Dir(), 0o700); err != nil {
		t.Fatal(err)
	}
	id := NewID()
	if err := os.WriteFile(pathFor(id), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Forget it in-process, so only the on-disk check can reject it.
	idMu.Lock()
	delete(idIssued, id)
	idMu.Unlock()
	for i := 0; i < 200; i++ {
		if got := NewID(); got == id {
			t.Fatalf("reissued %s, which already exists on disk", id)
		}
	}
}

func TestLoadUsesFilenameIdentity(t *testing.T) {
	t.Setenv("TAPIOCA_DATA_DIR", t.TempDir())
	s := &Session{ID: "original", Name: "n"}
	if err := s.Save(); err != nil {
		t.Fatal(err)
	}
	src, _ := os.ReadFile(pathFor("original"))
	if err := os.WriteFile(filepath.Join(Dir(), "backup.json"), src, 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := Load("backup")
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != "backup" {
		t.Fatalf("copied session kept embedded id %q; saving it would clobber the original", got.ID)
	}
}
