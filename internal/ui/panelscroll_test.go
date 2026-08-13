package ui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

// The settings panel has ten rows and is routinely given two lines of space.
// Everything past the second was cut off with no indication and no way to
// reach it.
func TestEverySettingsRowCanBeReached(t *testing.T) {
	// The default six-panel dashboard, where settings gets a few lines for its
	// ten rows — the situation in the report.
	m := dashApp(t, 100, 46)
	defs := m.fittedPanels()
	idx := panelIndex(t, defs, "settings")
	m.focus = focusDash
	m.dashPanelSel = idx
	m.dashEditing = true

	total, visible := m.panelContentHeight(defs, idx)
	if total <= visible {
		t.Fatalf("settings has %d lines in %d — the test is not exercising scrolling", total, visible)
	}

	a := m.mgr.ActiveAgent()
	seen := map[string]bool{}
	for i := range settingsRows {
		m.dashSel = i
		m.revealSettingsRow(defs)
		off := m.panelScrollFor("settings", total, visible)
		lines := fitLinesFrom(defs[idx].render(m, a, 30, visible), 30, visible, off)
		for _, l := range lines {
			if strings.Contains(l, settingsRows[i].label) {
				seen[settingsRows[i].key] = true
			}
		}
	}
	for _, r := range settingsRows {
		if !seen[r.key] {
			t.Errorf("row %q is never on screen while it is selected", r.label)
		}
	}
}

// A panel that fits must not scroll, and must not claim there is more.
func TestPanelThatFitsDoesNotScroll(t *testing.T) {
	m := dashApp(t, 100, 46)
	if got := scrollMark(0, 3, 10); got != "" {
		t.Errorf("scrollMark on content that fits = %q, want empty", got)
	}
	if m.scrollPanel("tokens", 1, 3, 10) {
		t.Error("a panel that fits was scrolled")
	}
	if off := m.panelScrollFor("tokens", 3, 10); off != 0 {
		t.Errorf("offset = %d, want 0", off)
	}
}

// The mark has to say which way, and read as direction without colour.
func TestScrollMarkShowsDirection(t *testing.T) {
	SetGlyphs("ascii")
	defer SetGlyphs("unicode")

	if got := scrollMark(0, 20, 5); got != "v" {
		t.Errorf("at the top: %q, want v", got)
	}
	if got := scrollMark(15, 20, 5); got != "^" {
		t.Errorf("at the bottom: %q, want ^", got)
	}
	if got := scrollMark(7, 20, 5); got != "^v" {
		t.Errorf("in the middle: %q, want ^v", got)
	}
}

// Scrolling must clamp: a panel that shrinks under a stale offset would
// otherwise render past its end and show nothing.
func TestScrollOffsetIsClampedToContent(t *testing.T) {
	m := dashApp(t, 100, 46)
	m.dashScroll = map[string]int{"tools": 500}
	if off := m.panelScrollFor("tools", 12, 5); off != 7 {
		t.Errorf("offset = %d, want it clamped to 7", off)
	}
	m.dashScroll["tools"] = -20
	if off := m.panelScrollFor("tools", 12, 5); off != 0 {
		t.Errorf("negative offset = %d, want 0", off)
	}
}

// fitLinesFrom must never read past the slice, whatever offset it is given.
func TestFitLinesFromNeverReadsPastTheEnd(t *testing.T) {
	lines := []string{"a", "b", "c"}
	for _, off := range []int{0, 1, 2, 3, 4, 100} {
		got := fitLinesFrom(lines, 10, 2, off)
		if len(got) != 2 {
			t.Errorf("offset %d returned %d lines, want 2", off, len(got))
		}
	}
}

// The wheel over a panel scrolls that panel and not the transcript behind it.
func TestWheelOverAPanelScrollsThatPanel(t *testing.T) {
	m := dashApp(t, 100, 46)
	defs := m.fittedPanels()
	idx := panelIndex(t, defs, "settings")
	total, visible := m.panelContentHeight(defs, idx)
	if total <= visible {
		t.Fatalf("settings has %d lines in %d — nothing to scroll", total, visible)
	}

	// A row inside the settings panel.
	dashW, _ := m.dashDims()
	_, sizes, _ := m.dashLayout(dashW, m.h-3)
	y := 2
	for i := 0; i < idx; i++ {
		y += sizes[i]
	}
	y += sizes[idx] / 2
	x := m.w - dashW/2
	before := m.panelScrollFor("settings", total, visible)

	m.Update(tea.MouseMsg{X: x, Y: y, Action: tea.MouseActionPress, Button: tea.MouseButtonWheelDown})

	if after := m.panelScrollFor("settings", total, visible); after == before {
		t.Errorf("the wheel over the settings panel did not scroll it (offset stayed %d)", before)
	}
}

// panelIndex finds a panel by key among the ones actually drawn.
func panelIndex(t *testing.T, defs []*panelDef, key string) int {
	t.Helper()
	for i, d := range defs {
		if d.key == key {
			return i
		}
	}
	t.Fatalf("the %s panel is not on screen at this size", key)
	return -1
}
