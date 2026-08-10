package session

import (
	"path/filepath"
	"testing"
	"time"
)

func TestForProjectSplitsByDirectory(t *testing.T) {
	metas := []Meta{
		{ID: "a", Cwd: "/home/k/one"},
		{ID: "b", Cwd: "/home/k/two"},
		{ID: "c", Cwd: "/home/k/one/"},   // trailing slash is the same place
		{ID: "d", Cwd: ""},               // saved before directories were recorded
		{ID: "e", Cwd: "/home/k/oneish"}, // prefix, not the same directory
	}
	here, elsewhere := ForProject(metas, "/home/k/one")
	if len(here) != 2 || here[0].ID != "a" || here[1].ID != "c" {
		t.Fatalf("here = %+v", here)
	}
	if len(elsewhere) != 3 {
		t.Fatalf("elsewhere = %+v", elsewhere)
	}
	// An unknown directory must not be claimed by whichever project is open.
	for _, m := range here {
		if m.ID == "d" {
			t.Error("a session with no recorded directory was claimed")
		}
	}
}

func TestLatestIDPrefersTheCurrentProject(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("TAPIOCA_DATA_DIR", dir)
	mine := filepath.Join(dir, "mine")
	other := filepath.Join(dir, "other")

	older := time.Now().Add(-time.Hour)
	for _, s := range []*Session{
		{ID: "mine-old", Cwd: mine, CreatedAt: older},
		{ID: "other-new", Cwd: other, CreatedAt: time.Now()},
	} {
		if err := s.Save(); err != nil {
			t.Fatal(err)
		}
		time.Sleep(10 * time.Millisecond) // Save stamps UpdatedAt
	}

	// other-new is the newest overall, but we are in `mine`.
	got, err := LatestID(mine)
	if err != nil {
		t.Fatal(err)
	}
	if got != "mine-old" {
		t.Errorf("-c resumed %q, want the session from this project", got)
	}

	// In a project with no sessions, fall back to the newest anywhere rather
	// than refusing to continue.
	got, err = LatestID(filepath.Join(dir, "elsewhere"))
	if err != nil {
		t.Fatal(err)
	}
	if got != "other-new" {
		t.Errorf("fallback resumed %q, want the newest overall", got)
	}
}
