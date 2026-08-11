package session

import (
	"os"
	"testing"
	"time"

	"tapioca/internal/provider"
)

func msg(role, text string) provider.Message {
	return provider.Message{Role: role, Blocks: []provider.Block{{Type: "text", Text: text}}}
}

// newSaved returns a session already written once, so the next Save is judged
// against a known snapshot.
func newSaved(t *testing.T, msgs ...provider.Message) *Session {
	t.Helper()
	t.Setenv("TAPIOCA_DATA_DIR", t.TempDir())
	s := &Session{
		ID: NewID(), Name: "test", CreatedAt: time.Now().Truncate(time.Second),
		Agents: []AgentState{{Name: "agent-1", Model: "m", Messages: msgs}},
	}
	if err := s.Save(); err != nil {
		t.Fatal(err)
	}
	return s
}

// snapBytes is how these tests tell a rewrite from an append: a rewrite always
// changes the content, since UpdatedAt moves with it.
func snapBytes(t *testing.T, s *Session) []byte {
	t.Helper()
	data, err := os.ReadFile(pathFor(s.ID))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func snapSize(t *testing.T, s *Session) int64 { return int64(len(snapBytes(t, s))) }

func journalSize(t *testing.T, s *Session) int64 {
	t.Helper()
	info, err := os.Stat(journalPath(s.ID))
	if os.IsNotExist(err) {
		return 0
	}
	if err != nil {
		t.Fatal(err)
	}
	return info.Size()
}

// The whole point: appending a message must not rewrite the snapshot.
func TestAppendGoesToJournal(t *testing.T) {
	s := newSaved(t, msg("user", "hello"))
	before := string(snapBytes(t, s))

	s.Agents[0].Messages = append(s.Agents[0].Messages, msg("assistant", "hi"))
	if err := s.Save(); err != nil {
		t.Fatal(err)
	}
	if string(snapBytes(t, s)) != before {
		t.Fatal("snapshot rewritten on a plain append")
	}
	if journalSize(t, s) == 0 {
		t.Fatal("nothing written to the journal")
	}

	got, err := Load(s.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Agents[0].Messages) != 2 || got.Agents[0].Messages[1].Text() != "hi" {
		t.Fatalf("journal not replayed: %+v", got.Agents[0].Messages)
	}
}

// The autosave tick fires every 30s whether or not anything happened.
func TestUnchangedSaveWritesNothing(t *testing.T) {
	s := newSaved(t, msg("user", "hello"))
	before := string(snapBytes(t, s))
	for i := 0; i < 3; i++ {
		if err := s.Save(); err != nil {
			t.Fatal(err)
		}
	}
	if string(snapBytes(t, s)) != before {
		t.Fatal("snapshot rewritten with nothing to save")
	}
	if journalSize(t, s) != 0 {
		t.Fatal("journal written with nothing to save")
	}
}

// /compact, /edit and /regen all rewrite history rather than extend it, and a
// journal cannot express that.
func TestHistoryRewriteForcesSnapshot(t *testing.T) {
	cases := map[string]func(*Session){
		"truncated": func(s *Session) { s.Agents[0].Messages = s.Agents[0].Messages[:1] },
		"replaced": func(s *Session) {
			s.Agents[0].Messages[1] = msg("assistant", "different answer")
		},
		"compacted": func(s *Session) {
			s.Agents[0].Messages = []provider.Message{msg("user", "summary")}
		},
		"settings changed": func(s *Session) { s.Agents[0].Model = "other-model" },
		"agent added": func(s *Session) {
			s.Agents = append(s.Agents, AgentState{Name: "agent-2", Model: "m"})
		},
		"renamed": func(s *Session) { s.Name = "renamed" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			s := newSaved(t, msg("user", "hello"), msg("assistant", "hi"))
			// A journal entry first, so the test also proves it is dropped.
			s.Agents[0].Messages = append(s.Agents[0].Messages, msg("user", "more"))
			if err := s.Save(); err != nil {
				t.Fatal(err)
			}
			if journalSize(t, s) == 0 {
				t.Fatal("expected an appended journal to start from")
			}
			before := string(snapBytes(t, s))

			mutate(s)
			if err := s.Save(); err != nil {
				t.Fatal(err)
			}
			if string(snapBytes(t, s)) == before {
				t.Fatal("snapshot not rewritten")
			}
			if journalSize(t, s) != 0 {
				t.Fatal("stale journal left behind; it would replay onto the new snapshot")
			}
			got, err := Load(s.ID)
			if err != nil {
				t.Fatal(err)
			}
			if len(got.Agents) != len(s.Agents) || len(got.Agents[0].Messages) != len(s.Agents[0].Messages) {
				t.Fatalf("reloaded %d agents / %d messages, want %d / %d",
					len(got.Agents), len(got.Agents[0].Messages), len(s.Agents), len(s.Agents[0].Messages))
			}
		})
	}
}

// Stats and todos move every turn without history changing.
func TestTailChangeJournals(t *testing.T) {
	s := newSaved(t, msg("user", "hello"))
	before := string(snapBytes(t, s))
	s.Agents[0].Stats.OutputTokens = 512
	s.Agents[0].Todos = []TodoItem{{Content: "ship it", Status: "in_progress"}}
	s.Agents[0].CtxTokens = 4096
	if err := s.Save(); err != nil {
		t.Fatal(err)
	}
	if string(snapBytes(t, s)) != before {
		t.Fatal("snapshot rewritten for a tail-only change")
	}
	got, err := Load(s.ID)
	if err != nil {
		t.Fatal(err)
	}
	a := got.Agents[0]
	if a.Stats.OutputTokens != 512 || a.CtxTokens != 4096 || len(a.Todos) != 1 {
		t.Fatalf("tail not replayed: %+v", a)
	}
}

// Left unbounded the journal would make loading slower than the rewrite it
// avoided.
func TestJournalCompacts(t *testing.T) {
	big := make([]byte, 8<<10)
	for i := range big {
		big[i] = 'x'
	}
	s := newSaved(t, msg("user", string(big)))
	for i := 0; i < 40; i++ {
		s.Agents[0].Messages = append(s.Agents[0].Messages, msg("assistant", string(big)))
		if err := s.Save(); err != nil {
			t.Fatal(err)
		}
		limit := int64(journalFloor)
		if half := snapSize(t, s) / 2; half > limit {
			limit = half
		}
		if got := journalSize(t, s); got > limit {
			t.Fatalf("journal %d bytes, over the %d limit after %d turns", got, limit, i+1)
		}
	}
	got, err := Load(s.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Agents[0].Messages) != 41 {
		t.Fatalf("loaded %d messages, want 41", len(got.Agents[0].Messages))
	}
}

// Whatever wrote the file outside this process knows something we do not, so
// appending our idea of the delta to it would invent a session.
func TestExternalEditForcesSnapshot(t *testing.T) {
	s := newSaved(t, msg("user", "hello"))
	other := *s
	other.Agents = []AgentState{{Name: "agent-1", Model: "m", Messages: []provider.Message{
		msg("user", "hello"), msg("assistant", "written by someone else"),
	}}}
	if err := writeRaw(&other); err != nil {
		t.Fatal(err)
	}

	s.Agents[0].Messages = append(s.Agents[0].Messages, msg("assistant", "ours"))
	if err := s.Save(); err != nil {
		t.Fatal(err)
	}
	if journalSize(t, s) != 0 {
		t.Fatal("appended to a file this process did not write")
	}
	got, err := Load(s.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Agents[0].Messages) != 2 || got.Agents[0].Messages[1].Text() != "ours" {
		t.Fatalf("unexpected history: %+v", got.Agents[0].Messages)
	}
}

// writeRaw writes a snapshot without touching the saved-state bookkeeping,
// standing in for another process.
func writeRaw(s *Session) error {
	saved := savedAll[s.ID]
	err := s.writeSnapshot()
	savedMu.Lock()
	savedAll[s.ID] = saved
	savedMu.Unlock()
	return err
}

// A crash mid-append leaves a partial line; the session must still open.
func TestTornJournalLineIgnored(t *testing.T) {
	s := newSaved(t, msg("user", "hello"))
	s.Agents[0].Messages = append(s.Agents[0].Messages, msg("assistant", "hi"))
	if err := s.Save(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(journalPath(s.ID))
	if err != nil {
		t.Fatal(err)
	}
	torn := append(append([]byte{}, data...), []byte(`{"updated_at":"2026-`)...)
	if err := os.WriteFile(journalPath(s.ID), torn, 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := Load(s.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Agents[0].Messages) != 2 {
		t.Fatalf("loaded %d messages, want the 2 that were written whole", len(got.Agents[0].Messages))
	}
}

// The picker keys its cache on the snapshot, which an append never touches.
func TestListSeesJournaledMessages(t *testing.T) {
	s := newSaved(t, msg("user", "hello"))
	if metas, err := List(); err != nil || len(metas) != 1 || metas[0].Messages != 1 {
		t.Fatalf("first listing: %v %+v", err, metas)
	}
	s.Agents[0].Messages = append(s.Agents[0].Messages, msg("assistant", "journaled reply"))
	if err := s.Save(); err != nil {
		t.Fatal(err)
	}
	metas, err := ListWithText()
	if err != nil {
		t.Fatal(err)
	}
	if len(metas) != 1 || metas[0].Messages != 2 {
		t.Fatalf("stale summary: %+v", metas)
	}
	if !contains(metas[0].Blob, "journaled reply") {
		t.Fatalf("search text missing the journaled message: %q", metas[0].Blob)
	}
}

func TestOrphanJournalRemoved(t *testing.T) {
	s := newSaved(t, msg("user", "hello"))
	s.Agents[0].Messages = append(s.Agents[0].Messages, msg("assistant", "hi"))
	if err := s.Save(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(pathFor(s.ID)); err != nil {
		t.Fatal(err)
	}
	if _, err := List(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(journalPath(s.ID)); !os.IsNotExist(err) {
		t.Fatal("orphaned journal kept")
	}
}
