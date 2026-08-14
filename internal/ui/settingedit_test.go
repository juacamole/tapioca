package ui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"tapioca/internal/agent"
)

// settingsApp puts the dashboard into settings-editing mode on a given row.
func settingsApp(t *testing.T, rowKey string) (*App, *agent.Agent) {
	t.Helper()
	m := dashApp(t, 100, 46)
	m.cfg.Dashboard.Panels = []string{"settings"}
	m.focus = focusDash
	m.dashPanelSel = 0
	m.dashEditing = true
	for i, r := range settingsRows {
		if r.key == rowKey {
			m.dashSel = i
		}
	}
	a := m.mgr.ActiveAgent()
	a.MaxTokens = m.cfg.MaxTokens
	a.ThinkingBudget = m.cfg.ThinkingBudget
	a.Temperature = m.cfg.Temperature
	return m, a
}

func typeKeys(m *App, s string) {
	for _, r := range s {
		m.Update(tea.KeyPressMsg{Code: r, Text: string(r)})
	}
}

func enter(m *App) { m.Update(tea.KeyPressMsg{Code: tea.KeyEnter}) }
func esc(m *App)   { m.Update(tea.KeyPressMsg{Code: tea.KeyEscape}) }

// Reaching 32000 from 4096 by stepping is fifty-five presses of an arrow key.
func TestMaxTokensCanBeTyped(t *testing.T) {
	m, a := settingsApp(t, "max_tokens")

	enter(m) // open the field
	if m.setEdit == nil {
		t.Fatal("enter on max tokens did not open a typed field")
	}
	// Clear the seeded value, then type.
	for range 8 {
		m.Update(tea.KeyPressMsg{Code: tea.KeyBackspace})
	}
	typeKeys(m, "32000")
	enter(m)

	if m.setEdit != nil {
		t.Fatalf("the field is still open: %q", m.setEdit.err)
	}
	if a.MaxTokens != 32000 {
		t.Errorf("MaxTokens = %d, want 32000", a.MaxTokens)
	}
	if m.cfg.MaxTokens != 32000 {
		t.Errorf("config MaxTokens = %d, want 32000", m.cfg.MaxTokens)
	}
}

func TestThinkBudgetCanBeTyped(t *testing.T) {
	m, a := settingsApp(t, "budget")
	enter(m)
	for range 8 {
		m.Update(tea.KeyPressMsg{Code: tea.KeyBackspace})
	}
	typeKeys(m, "16384")
	enter(m)

	if a.ThinkingBudget != 16384 {
		t.Errorf("ThinkingBudget = %d, want 16384", a.ThinkingBudget)
	}
}

// Nonsense is refused with a reason, and what was typed survives so a near
// miss can be corrected rather than retyped.
func TestNonNumericEntryIsRefusedAndKept(t *testing.T) {
	m, a := settingsApp(t, "max_tokens")
	before := a.MaxTokens

	enter(m)
	for range 8 {
		m.Update(tea.KeyPressMsg{Code: tea.KeyBackspace})
	}
	typeKeys(m, "abc")
	enter(m)

	if m.setEdit == nil {
		t.Fatal("the field closed on a bad value")
	}
	if m.setEdit.err == "" {
		t.Error("no reason was given")
	}
	if m.setEdit.input.Value() != "abc" {
		t.Errorf("the typed text was lost: %q", m.setEdit.input.Value())
	}
	if a.MaxTokens != before {
		t.Errorf("MaxTokens changed to %d despite the bad entry", a.MaxTokens)
	}
}

// Out of range is refused too, and the message names the range rather than
// leaving the user to guess.
func TestOutOfRangeEntryIsRefusedWithTheRange(t *testing.T) {
	m, a := settingsApp(t, "max_tokens")
	before := a.MaxTokens

	enter(m)
	for range 8 {
		m.Update(tea.KeyPressMsg{Code: tea.KeyBackspace})
	}
	typeKeys(m, "5")
	enter(m)

	if m.setEdit == nil {
		t.Fatal("the field closed on an out-of-range value")
	}
	if !strings.Contains(m.setEdit.err, "256") {
		t.Errorf("the message does not name the minimum: %q", m.setEdit.err)
	}
	if a.MaxTokens != before {
		t.Errorf("MaxTokens changed to %d", a.MaxTokens)
	}
}

// Escape leaves the value as it was.
func TestEscapeLeavesTheValueAlone(t *testing.T) {
	m, a := settingsApp(t, "max_tokens")
	before := a.MaxTokens

	enter(m)
	for range 8 {
		m.Update(tea.KeyPressMsg{Code: tea.KeyBackspace})
	}
	typeKeys(m, "9999")
	esc(m)

	if m.setEdit != nil {
		t.Error("escape did not close the field")
	}
	if a.MaxTokens != before {
		t.Errorf("MaxTokens = %d after escape, want %d", a.MaxTokens, before)
	}
}

// Digits must not leak past the field and step the value underneath at the
// same time.
func TestTypingDoesNotAlsoStepTheValue(t *testing.T) {
	m, a := settingsApp(t, "max_tokens")
	enter(m)
	before := a.MaxTokens
	typeKeys(m, "1")
	if a.MaxTokens != before {
		t.Errorf("typing changed the live value to %d", a.MaxTokens)
	}
}

// Booleans and lists keep stepping: there is nothing to type for them.
func TestNonNumericRowsStillToggle(t *testing.T) {
	for _, key := range []string{"thinking", "tools", "verbose", "dashpos"} {
		m, _ := settingsApp(t, key)
		enter(m)
		if m.setEdit != nil {
			t.Errorf("%s opened a typed field", key)
			m.setEdit = nil
		}
	}
}

// A thousands separator is how the value is displayed, so it has to be
// accepted back.
func TestTypedValueAcceptsThousandsSeparators(t *testing.T) {
	m, a := settingsApp(t, "max_tokens")
	enter(m)
	for range 8 {
		m.Update(tea.KeyPressMsg{Code: tea.KeyBackspace})
	}
	typeKeys(m, "8,192")
	enter(m)
	if a.MaxTokens != 8192 {
		t.Errorf("MaxTokens = %d, want 8192", a.MaxTokens)
	}
}

// The field opens seeded with the current value, so a small correction is a
// keystroke rather than a retype.
func TestFieldOpensWithTheCurrentValue(t *testing.T) {
	m, a := settingsApp(t, "max_tokens")
	a.MaxTokens = 4096
	m.cfg.MaxTokens = 4096
	enter(m)
	if m.setEdit == nil {
		t.Fatal("no field opened")
	}
	if got := m.setEdit.input.Value(); got != "4096" {
		t.Errorf("field opened with %q, want 4096 with no separator", got)
	}
}

// Escape reached a global handler before the dashboard ever saw it, so the
// settings panel's own "esc done" hint described something that did not
// happen. Editing mode has to be leavable with the key it advertises.
func TestEscapeLeavesEditingMode(t *testing.T) {
	m, _ := settingsApp(t, "thinking")
	if !m.dashEditing {
		t.Fatal("not editing")
	}
	esc(m)
	if m.dashEditing {
		t.Error("escape did not leave editing mode")
	}
}

// Two escapes: the field, then editing mode.
func TestEscapeClosesTheFieldThenEditing(t *testing.T) {
	m, _ := settingsApp(t, "max_tokens")
	enter(m)
	if m.setEdit == nil {
		t.Fatal("no field opened")
	}
	esc(m)
	if m.setEdit != nil {
		t.Error("the first escape did not close the field")
	}
	if !m.dashEditing {
		t.Error("the first escape also left editing mode; it should close one thing at a time")
	}
	esc(m)
	if m.dashEditing {
		t.Error("the second escape did not leave editing mode")
	}
}
