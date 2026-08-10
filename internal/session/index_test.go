package session

import (
	"os"
	"testing"
	"time"

	"tapioca/internal/provider"
)

func save(t *testing.T, id, name, cwd, text string) {
	t.Helper()
	s := &Session{ID: id, Name: name, Cwd: cwd, CreatedAt: time.Now(),
		Agents: []AgentState{{Name: "a", Messages: []provider.Message{
			provider.TextMessage("user", text),
		}}}}
	if err := s.Save(); err != nil {
		t.Fatal(err)
	}
}

func TestListUsesTheCacheButNoticesChanges(t *testing.T) {
	t.Setenv("TAPIOCA_DATA_DIR", t.TempDir())
	save(t, "one", "first", "/p", "hello there")

	metas, err := List()
	if err != nil || len(metas) != 1 || metas[0].Name != "first" {
		t.Fatalf("first listing: %+v %v", metas, err)
	}
	if _, err := os.Stat(indexPath()); err != nil {
		t.Fatalf("index not written: %v", err)
	}

	// A rename must be picked up: the file changed, so the summary is stale.
	save(t, "one", "renamed", "/p", "hello there")
	metas, _ = List()
	if len(metas) != 1 || metas[0].Name != "renamed" {
		t.Fatalf("a changed session was served from cache: %+v", metas)
	}

	// A deleted session must leave the index.
	if err := os.Remove(pathFor("one")); err != nil {
		t.Fatal(err)
	}
	metas, _ = List()
	if len(metas) != 0 {
		t.Fatalf("deleted session still listed: %+v", metas)
	}
	if idx := loadIndex(); len(idx.Entries) != 0 {
		t.Errorf("index still holds %d entries", len(idx.Entries))
	}
}

// The index and search files live in the same directory as the sessions and
// must not be mistaken for sessions themselves.
func TestIndexFilesAreNotListedAsSessions(t *testing.T) {
	t.Setenv("TAPIOCA_DATA_DIR", t.TempDir())
	save(t, "real", "a session", "/p", "text")
	List() // writes index.json and search.json
	metas, err := List()
	if err != nil {
		t.Fatal(err)
	}
	if len(metas) != 1 || metas[0].ID != "real" {
		t.Fatalf("listing picked up its own bookkeeping: %+v", metas)
	}
}

// Search text is only paid for when the picker asks for it.
func TestSearchTextIsSeparateFromTheIndex(t *testing.T) {
	t.Setenv("TAPIOCA_DATA_DIR", t.TempDir())
	save(t, "one", "n", "/p", "a distinctive phrase")

	metas, _ := List()
	if len(metas) != 1 || metas[0].Blob != "" {
		t.Fatalf("List should not carry search text: %q", metas[0].Blob)
	}
	withText, err := ListWithText()
	if err != nil {
		t.Fatal(err)
	}
	if len(withText) != 1 || withText[0].Blob == "" {
		t.Fatalf("ListWithText returned no text: %+v", withText)
	}
	if !contains(withText[0].Blob, "a distinctive phrase") {
		t.Errorf("search text wrong: %q", withText[0].Blob)
	}
}

// A corrupt or older cache must not break listing.
func TestBadIndexIsRebuilt(t *testing.T) {
	t.Setenv("TAPIOCA_DATA_DIR", t.TempDir())
	save(t, "one", "n", "/p", "text")
	List()
	if err := os.WriteFile(indexPath(), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	metas, err := List()
	if err != nil || len(metas) != 1 {
		t.Fatalf("a corrupt index broke listing: %+v %v", metas, err)
	}
}

func TestSessionsAddedBehindOurBackAppear(t *testing.T) {
	t.Setenv("TAPIOCA_DATA_DIR", t.TempDir())
	save(t, "one", "first", "/p", "text")
	List()
	// A second Tapioca, or a restored backup, drops a file in directly.
	save(t, "two", "second", "/p", "text")
	metas, _ := List()
	if len(metas) != 2 {
		t.Fatalf("new session not noticed: %+v", metas)
	}
}

func contains(haystack, needle string) bool {
	return len(haystack) >= len(needle) && (haystack == needle ||
		len(needle) == 0 || indexOf(haystack, needle) >= 0)
}

func indexOf(h, n string) int {
	for i := 0; i+len(n) <= len(h); i++ {
		if h[i:i+len(n)] == n {
			return i
		}
	}
	return -1
}
