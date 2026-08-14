package ui

import (
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"
)

// There was no way to find anything in a long session. The transcript
// scrolled, and that was all it did — after an hour of work, locating a
// command the agent ran two hundred lines ago meant scrolling and reading.

// searchHit is one match: a line in the transcript and the rune range on it.
type searchHit struct {
	line     int
	from, to int // rune offsets, half-open
}

// searchState is an open search over the transcript.
type searchState struct {
	query string
	hits  []searchHit
	sel   int // index into hits
	// prevOffset is where the transcript was before searching, restored on
	// close so an abandoned search does not also lose your place.
	prevOffset int
}

// openSearch starts a search, remembering where the transcript was.
func (m *App) openSearch() {
	m.search = &searchState{prevOffset: m.vp.YOffset()}
}

// closeSearch drops the search and puts the transcript back the way it was.
func (m *App) closeSearch() {
	if m.search == nil {
		return
	}
	off := m.search.prevOffset
	m.search = nil
	m.clearSelection() // restores chatStyled, dropping the highlights
	m.vp.SetYOffset(off)
}

// runSearch recomputes the matches for the current query. Matching runs on the
// plain projection, so a match is never thrown off by the styling around it —
// the same reason mouse selection uses it.
func (m *App) runSearch() {
	st := m.search
	if st == nil {
		return
	}
	st.hits = nil
	st.sel = 0
	q := strings.ToLower(st.query)
	if q == "" {
		m.clearSelection()
		return
	}
	for i, line := range m.plainLines() {
		lower := strings.ToLower(line)
		runes := []rune(line)
		// Byte offsets from Index have to become rune offsets, or a match
		// after any multi-byte character highlights the wrong span.
		for at := 0; ; {
			j := strings.Index(lower[at:], q)
			if j < 0 {
				break
			}
			start := len([]rune(lower[:at+j]))
			st.hits = append(st.hits, searchHit{
				line: i,
				from: start,
				to:   start + len([]rune(q)),
			})
			at += j + len(q)
			if at >= len(lower) {
				break
			}
		}
		_ = runes
	}
	m.applySearchHighlight()
	m.scrollToHit()
}

// applySearchHighlight redraws the transcript with every match marked and the
// selected one marked differently, so "which of the twelve am I on" is
// answerable without counting.
func (m *App) applySearchHighlight() {
	st := m.search
	if st == nil {
		return
	}
	if len(st.hits) == 0 {
		m.clearSelection()
		return
	}
	lines := make([]string, len(m.chatStyled))
	copy(lines, m.chatStyled)
	plain := m.plainLines()

	for i, h := range st.hits {
		if h.line >= len(plain) {
			continue
		}
		runes := []rune(plain[h.line])
		from, to := min(h.from, len(runes)), min(h.to, len(runes))
		if from >= to {
			continue
		}
		style := styWarn
		if i == st.sel {
			style = stySelected
		}
		// The whole line is replaced with its plain form plus the mark. A
		// styled line cannot be spliced by rune offset without decoding it,
		// and losing colour on matching lines is the same trade selection
		// already makes.
		lines[h.line] = string(runes[:from]) +
			style.Render(string(runes[from:to])) +
			string(runes[to:])
	}
	off := m.vp.YOffset()
	m.vp.SetContent(strings.Join(lines, "\n"))
	m.vp.SetYOffset(off)
}

// scrollToHit brings the selected match into view.
func (m *App) scrollToHit() {
	st := m.search
	if st == nil || len(st.hits) == 0 {
		return
	}
	line := st.hits[st.sel].line
	h := m.vp.Height()
	if h < 1 {
		return
	}
	// Put the match a third of the way down rather than at the very top, so
	// there is context above it.
	target := max(0, line-h/3)
	m.vp.SetYOffset(target)
}

// stepSearch moves to the next or previous match, wrapping.
func (m *App) stepSearch(dir int) {
	st := m.search
	if st == nil || len(st.hits) == 0 {
		return
	}
	n := len(st.hits)
	st.sel = ((st.sel+dir)%n + n) % n
	m.applySearchHighlight()
	m.scrollToHit()
}

// handleSearchKey drives search while it is open. It owns the keyboard: every
// printable key is part of the query.
func (m *App) handleSearchKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch {
	case m.keys.Is(msg, "cancel"):
		m.closeSearch()
		return m, nil
	case m.keys.Is(msg, "send"):
		// Enter keeps the position and leaves search, which is what you want
		// after finding the thing.
		if m.search != nil {
			m.search = nil
		}
		return m, nil
	}
	switch msg.String() {
	case "backspace":
		if q := []rune(m.search.query); len(q) > 0 {
			m.search.query = string(q[:len(q)-1])
			m.runSearch()
		}
		return m, nil
	case "down", "ctrl+n":
		m.stepSearch(1)
		return m, nil
	case "up", "ctrl+p":
		m.stepSearch(-1)
		return m, nil
	}
	if msg.Text != "" {
		m.search.query += msg.Text
		m.runSearch()
	}
	return m, nil
}

// searchStatus is the line shown while searching.
func (m *App) searchStatus() string {
	st := m.search
	if st == nil {
		return ""
	}
	q := st.query
	if q == "" {
		q = styDim.Render("type to search")
	}
	switch {
	case st.query == "":
		return " " + styAccent.Render("search") + " " + q + styDim.Render(gl.sep+"esc closes")
	case len(st.hits) == 0:
		return " " + styAccent.Render("search") + " " + q + " " +
			styErr.Render("no matches") + styDim.Render(gl.sep+"esc closes")
	default:
		return " " + styAccent.Render("search") + " " + q + " " +
			styDim.Render(fmt.Sprintf("%d/%d"+gl.sep+"up/down walk"+gl.sep+"enter keeps"+gl.sep+"esc closes",
				st.sel+1, len(st.hits)))
	}
}
