package session

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"tapioca/internal/provider"
)

// The fingerprint hashed lengths, not contents, so an in-place edit of the
// same length at the same timestamp read as "nothing changed" — the save was
// skipped and what was on screen diverged from what was on disk, silently.
func TestSameLengthEditIsSaved(t *testing.T) {
	s := newSaved(t, msg("user", "AAAA"))
	s.Agents[0].Messages[0].Blocks[0].Text = "BBBB"
	s.Agents[0].Messages = append(s.Agents[0].Messages, msg("assistant", "next turn"))
	if err := s.Save(); err != nil {
		t.Fatal(err)
	}
	got, err := Load(s.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Agents[0].Messages[0].Text() != "BBBB" {
		t.Fatalf("edit lost: message 0 is %q", got.Agents[0].Messages[0].Text())
	}
	if len(got.Agents[0].Messages) != 2 {
		t.Fatalf("loaded %d messages, want 2", len(got.Agents[0].Messages))
	}
}

// Same length, same everything except one byte in a tool result.
func TestSameLengthToolResultEditIsSaved(t *testing.T) {
	m := provider.Message{Role: "user", Blocks: []provider.Block{
		{Type: "tool_result", Name: "bash", Content: "exit 0"},
	}}
	s := newSaved(t, m)
	s.Agents[0].Messages[0].Blocks[0].Content = "exit 1"
	if err := s.Save(); err != nil {
		t.Fatal(err)
	}
	got, err := Load(s.ID)
	if err != nil {
		t.Fatal(err)
	}
	if c := got.Agents[0].Messages[0].Blocks[0].Content; c != "exit 1" {
		t.Fatalf("edit lost: %q", c)
	}
}

// The snapshot is published before the journal is removed, so a crash in
// between leaves a journal the snapshot already contains. Replay must skip it
// rather than append those turns a second time.
func TestStaleJournalIsNotReplayedTwice(t *testing.T) {
	s := newSaved(t, msg("user", "one"))
	s.Agents[0].Messages = append(s.Agents[0].Messages, msg("assistant", "two"))
	if err := s.Save(); err != nil { // journal now holds "two"
		t.Fatal(err)
	}
	journal, err := os.ReadFile(journalPath(s.ID))
	if err != nil {
		t.Fatal(err)
	}
	// Force the full rewrite, which publishes a snapshot containing "two"...
	s.Agents[0].Model = "other"
	if err := s.Save(); err != nil {
		t.Fatal(err)
	}
	// ...then put the journal back, as a crash between rename and remove would.
	if err := os.WriteFile(journalPath(s.ID), journal, 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := Load(s.ID)
	if err != nil {
		t.Fatal(err)
	}
	if n := len(got.Agents[0].Messages); n != 2 {
		t.Fatalf("loaded %d messages, want 2 — the stale journal was replayed onto a snapshot that already had them", n)
	}
}

// A fixed "<id>.json.tmp" let a second instance truncate the file mid-write
// and publish the result, and inherited the mode of whatever was there.
func TestTempFileIsUniqueAndPrivate(t *testing.T) {
	s := newSaved(t, msg("user", "hello"))
	dir := Dir()

	// Something else already sitting at the obvious temp name, world-readable.
	squatter := pathFor(s.ID) + ".tmp"
	if err := os.WriteFile(squatter, []byte("{}"), 0o666); err != nil {
		t.Fatal(err)
	}
	s.Agents[0].Messages = append(s.Agents[0].Messages, msg("assistant", "hi"))
	s.Agents[0].Model = "force-a-snapshot"
	if err := s.Save(); err != nil {
		t.Fatal(err)
	}

	info, err := os.Stat(pathFor(s.ID))
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("session published with mode %o, want 600", perm)
	}
	if data, err := os.ReadFile(squatter); err != nil || string(data) != "{}" {
		t.Fatalf("the squatted temp name was reused: %v %q", err, data)
	}
	// And nothing of ours is left lying around.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), ".tmp") && filepath.Join(dir, e.Name()) != squatter {
			t.Errorf("left a temp file behind: %s", e.Name())
		}
	}
}

// The whole session must still round-trip after all of this.
func TestSnapshotStillRoundTrips(t *testing.T) {
	s := newSaved(t, msg("user", "hello"))
	s.Agents[0].Stats.Turns = 3
	s.Agents[0].Messages = append(s.Agents[0].Messages, msg("assistant", "hi"))
	if err := s.Save(); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(pathFor(s.ID))
	if err != nil {
		t.Fatal(err)
	}
	var check Session
	if err := json.Unmarshal(raw, &check); err != nil {
		t.Fatalf("published snapshot is not valid JSON: %v", err)
	}
	got, err := Load(s.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Agents[0].Messages) != 2 || got.Agents[0].Stats.Turns != 3 {
		t.Fatalf("round trip lost data: %+v", got.Agents[0])
	}
}
