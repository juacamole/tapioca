package ui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
)

// searchApp gives the transcript some content to find things in.
func searchApp(t *testing.T, lines ...string) *App {
	t.Helper()
	m := dashApp(t, 100, 30)
	m.chatStyled = lines
	m.chatPlain = nil // recomputed from chatStyled
	return m
}

func typeSearch(m *App, s string) {
	for _, r := range s {
		m.handleSearchKey(tea.KeyPressMsg{Code: r, Text: string(r)})
	}
}

// The basic thing that was missing: find a term and know how many there are.
func TestSearchFindsEveryOccurrence(t *testing.T) {
	m := searchApp(t,
		"first parseConfig call",
		"nothing here",
		"second parseConfig call",
		"and parseConfig again",
	)
	m.openSearch()
	typeSearch(m, "parseConfig")

	if got := len(m.search.hits); got != 3 {
		t.Fatalf("found %d matches, want 3", got)
	}
	if m.search.hits[0].line != 0 || m.search.hits[1].line != 2 || m.search.hits[2].line != 3 {
		t.Errorf("matches on the wrong lines: %+v", m.search.hits)
	}
}

// Two on one line is the case a naive scan misses.
func TestSearchFindsRepeatsOnOneLine(t *testing.T) {
	m := searchApp(t, "foo and foo and foo")
	m.openSearch()
	typeSearch(m, "foo")
	if got := len(m.search.hits); got != 3 {
		t.Errorf("found %d matches on one line, want 3", got)
	}
}

// Offsets are in runes, not bytes: a match after a multi-byte character would
// otherwise highlight the wrong span.
func TestSearchOffsetsAreRunesNotBytes(t *testing.T) {
	m := searchApp(t, "— — — target")
	m.openSearch()
	typeSearch(m, "target")

	if len(m.search.hits) != 1 {
		t.Fatalf("found %d matches, want 1", len(m.search.hits))
	}
	h := m.search.hits[0]
	runes := []rune(m.plainLines()[0])
	if string(runes[h.from:h.to]) != "target" {
		t.Errorf("the highlighted span is %q, not the match", string(runes[h.from:h.to]))
	}
}

func TestSearchIsCaseInsensitive(t *testing.T) {
	m := searchApp(t, "ParseConfig", "PARSECONFIG", "parseconfig")
	m.openSearch()
	typeSearch(m, "parseconfig")
	if got := len(m.search.hits); got != 3 {
		t.Errorf("found %d matches, want 3", got)
	}
}

// Walking past the last match wraps, so n keeps working rather than stopping.
func TestSearchWrapsAtBothEnds(t *testing.T) {
	m := searchApp(t, "a x", "b x", "c x")
	m.openSearch()
	typeSearch(m, "x")

	if m.search.sel != 0 {
		t.Fatalf("starts at %d, want 0", m.search.sel)
	}
	m.stepSearch(1)
	m.stepSearch(1)
	if m.search.sel != 2 {
		t.Fatalf("after two steps at %d, want 2", m.search.sel)
	}
	m.stepSearch(1)
	if m.search.sel != 0 {
		t.Errorf("did not wrap forward: at %d", m.search.sel)
	}
	m.stepSearch(-1)
	if m.search.sel != 2 {
		t.Errorf("did not wrap backward: at %d", m.search.sel)
	}
}

// Closing restores the position, so an abandoned search does not also lose
// your place in a long session.
func TestClosingSearchRestoresThePosition(t *testing.T) {
	m := searchApp(t, "top", "middle", "target", "more", "bottom")
	m.vp.SetYOffset(0)
	m.openSearch()
	typeSearch(m, "target")
	m.closeSearch()

	if m.search != nil {
		t.Error("search stayed open")
	}
	if m.vp.YOffset() != 0 {
		t.Errorf("the transcript is at %d, want the position it started from (0)", m.vp.YOffset())
	}
}

// A query matching nothing has to say so rather than looking like a no-op.
func TestSearchReportsNoMatches(t *testing.T) {
	m := searchApp(t, "nothing relevant")
	m.openSearch()
	typeSearch(m, "absent")

	if len(m.search.hits) != 0 {
		t.Fatal("found matches that are not there")
	}
	if !strings.Contains(stripAnsi(m.searchStatus()), "no matches") {
		t.Errorf("the status does not say there are none: %q", stripAnsi(m.searchStatus()))
	}
}

// Backspace narrows and widens the search rather than clearing it.
func TestSearchBackspaceReRuns(t *testing.T) {
	m := searchApp(t, "alpha", "alphabet")
	m.openSearch()
	typeSearch(m, "alphab")
	if len(m.search.hits) != 1 {
		t.Fatalf("%q matched %d lines, want 1", m.search.query, len(m.search.hits))
	}
	m.handleSearchKey(tea.KeyPressMsg{Code: tea.KeyBackspace})
	if len(m.search.hits) != 2 {
		t.Errorf("after backspace %q matched %d, want 2", m.search.query, len(m.search.hits))
	}
}

// The status line has to show where you are, or "12 matches" is useless.
func TestSearchStatusShowsPosition(t *testing.T) {
	m := searchApp(t, "x", "x", "x")
	m.openSearch()
	typeSearch(m, "x")
	if got := stripAnsi(m.searchStatus()); !strings.Contains(got, "1/3") {
		t.Errorf("status = %q, want it to show 1/3", got)
	}
	m.stepSearch(1)
	if got := stripAnsi(m.searchStatus()); !strings.Contains(got, "2/3") {
		t.Errorf("status = %q, want it to show 2/3", got)
	}
}
