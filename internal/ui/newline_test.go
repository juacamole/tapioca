package ui

import (
	"bytes"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

// shift+enter is the advertised newline key and has to stay that way: it is
// what the hint bar, the help screen and the README all name.
func TestShiftEnterIsTheAdvertisedNewlineKey(t *testing.T) {
	km := NewKeyMap(nil)
	if got := km.FirstKey("newline"); got != "shift+enter" {
		t.Errorf("newline is advertised as %q, want shift+enter", got)
	}
}

// The whole point: a shift+enter from the terminal must end up triggering the
// newline action. It arrives as an escape sequence, is translated on the way
// in, and only then meets the keymap — so both halves are checked together.
func TestShiftEnterFromTheTerminalTriggersNewline(t *testing.T) {
	km := NewKeyMap(nil)
	for _, seq := range shiftEnterSequences {
		out := make([]byte, 16)
		n, _ := ExtendedKeyReader(bytes.NewReader(seq)).Read(out)
		translated := out[:n]
		if !bytes.Equal(translated, []byte{newlineByte}) {
			t.Fatalf("%q translated to %q, want a single newline byte", seq, translated)
		}
		// That byte is what bubbletea reports as ctrl+j.
		if !km.Is(tea.KeyMsg{Type: tea.KeyCtrlJ}, "newline") {
			t.Error("the translated key does not trigger the newline action")
		}
	}
}

// ctrl+j is the same key on the wire and works with no protocol at all, so it
// stays bound as the fallback for terminals that ignore the request.
func TestCtrlJRemainsBound(t *testing.T) {
	if !NewKeyMap(nil).Is(tea.KeyMsg{Type: tea.KeyCtrlJ}, "newline") {
		t.Error("ctrl+j no longer inserts a newline")
	}
}

// No alt bindings anywhere: it is a project rule, not a preference.
func TestNoAltBindings(t *testing.T) {
	km := NewKeyMap(nil)
	for _, a := range actions {
		if strings.Contains(strings.ToLower(km.KeysFor(a.Name)), "alt+") {
			t.Errorf("action %q binds an alt key: %s", a.Name, km.KeysFor(a.Name))
		}
	}
}

// Plain enter must still send, or the newline binding has eaten submission.
func TestPlainEnterStillSends(t *testing.T) {
	km := NewKeyMap(nil)
	if km.Is(tea.KeyMsg{Type: tea.KeyEnter}, "newline") {
		t.Error("plain enter triggers newline — prompts could not be sent")
	}
	if !km.Is(tea.KeyMsg{Type: tea.KeyEnter}, "send") {
		t.Error("plain enter no longer sends")
	}
}
