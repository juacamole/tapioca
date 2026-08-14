package ui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
)

// shift+enter took three attempts. Terminals send the same byte for it as for
// enter, so it cannot be told apart unless the terminal encodes modifiers —
// and Bubbletea v1 could neither ask for that nor decode the answer. v2 does
// both, so this is a key like any other now.
func TestShiftEnterIsBoundToNewline(t *testing.T) {
	km := NewKeyMap(nil)
	shiftEnter := tea.KeyPressMsg{Code: tea.KeyEnter, Mod: tea.ModShift}

	if got := shiftEnter.String(); got != "shift+enter" {
		t.Fatalf("bubbletea names this key %q, so the binding would never match", got)
	}
	if !km.Is(shiftEnter, "newline") {
		t.Error("shift+enter does not insert a newline")
	}
	if got := km.FirstKey("newline"); got != "shift+enter" {
		t.Errorf("newline is advertised as %q, want shift+enter", got)
	}
}

// ctrl+j is the same key on the wire and needs no protocol at all, so it stays
// as the fallback for terminals without keyboard enhancements.
func TestCtrlJRemainsBoundToNewline(t *testing.T) {
	km := NewKeyMap(nil)
	ctrlJ := tea.KeyPressMsg{Code: 'j', Mod: tea.ModCtrl}
	if got := ctrlJ.String(); got != "ctrl+j" {
		t.Fatalf("ctrl+j is named %q", got)
	}
	if !km.Is(ctrlJ, "newline") {
		t.Error("ctrl+j no longer inserts a newline")
	}
}

// Plain enter must still send, or the newline binding has eaten submission.
func TestPlainEnterStillSends(t *testing.T) {
	km := NewKeyMap(nil)
	enterMsg := tea.KeyPressMsg{Code: tea.KeyEnter}
	if km.Is(enterMsg, "newline") {
		t.Error("plain enter triggers newline — prompts could not be sent")
	}
	if !km.Is(enterMsg, "send") {
		t.Error("plain enter no longer sends")
	}
}

// No alt bindings anywhere: it is a project rule.
func TestNoAltBindings(t *testing.T) {
	km := NewKeyMap(nil)
	for _, a := range actions {
		if strings.Contains(strings.ToLower(km.KeysFor(a.Name)), "alt+") {
			t.Errorf("action %q binds an alt key: %s", a.Name, km.KeysFor(a.Name))
		}
	}
}

// The alt screen and mouse mode are declared on the view in v2 rather than at
// startup, so losing those lines would silently change the whole UI.
func TestViewDeclaresScreenState(t *testing.T) {
	m := dashApp(t, 100, 30)
	v := m.View()
	if !v.AltScreen {
		t.Error("the view does not ask for the alt screen")
	}
	if v.MouseMode != tea.MouseModeCellMotion {
		t.Errorf("mouse mode = %v, want cell motion", v.MouseMode)
	}
}
