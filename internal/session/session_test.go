package session

import (
	"os"
	"path/filepath"
	"testing"
)

func TestNewIDUnique(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 50; i++ {
		id := NewID()
		if seen[id] {
			t.Fatalf("duplicate id %s", id)
		}
		seen[id] = true
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
