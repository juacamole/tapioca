package ui

import (
	"bytes"
	"os"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

// The advertised newline key has to be one that works with no terminal
// configuration at all. shift+enter is not that key: terminals send the same
// byte for it as for enter, and the flag that would separate them reports
// every key as an escape code, which bubbletea cannot decode.
func TestAdvertisedNewlineKeyWorksWithNoTerminalSetup(t *testing.T) {
	km := NewKeyMap(nil)
	if got := km.FirstKey("newline"); got != "ctrl+j" {
		t.Errorf("newline is advertised as %q; only ctrl+j arrives unaided", got)
	}
	if !km.Is(tea.KeyMsg{Type: tea.KeyCtrlJ}, "newline") {
		t.Error("ctrl+j does not trigger the newline action")
	}
}

// shift+enter stays bound for terminals that have been mapped to send it, and
// the sequence they send is translated to the newline byte on the way in.
func TestAMappedShiftEnterStillProducesANewline(t *testing.T) {
	km := NewKeyMap(nil)
	for _, seq := range shiftEnterSequences {
		out := make([]byte, 16)
		n, _ := ExtendedKeyReader(bytes.NewReader(seq)).Read(out)
		if got := out[:n]; !bytes.Equal(got, []byte{newlineByte}) {
			t.Fatalf("%q translated to %q, want a single newline byte", seq, got)
		}
	}
	if !km.Is(tea.KeyMsg{Type: tea.KeyCtrlJ}, "newline") {
		t.Error("the translated byte does not trigger the newline action")
	}
	if !strings.Contains(km.KeysFor("newline"), "shift+enter") {
		t.Error("shift+enter is no longer bound; a mapped terminal would do nothing")
	}
}

// The help must not promise shift+enter works on its own — that promise is
// what made this a bug report twice.
func TestNewlineHelpSaysAMappingIsNeeded(t *testing.T) {
	for _, a := range actions {
		if a.Name != "newline" {
			continue
		}
		if !strings.Contains(a.Help, "terminal mapping") {
			t.Errorf("the newline help does not say a mapping is needed: %q", a.Help)
		}
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

// Nothing may be written to the terminal to turn a keyboard protocol on.
// Asking for disambiguation moves ctrl+key and esc onto CSI-u, which bubbletea
// swallows — so keys that work today would quietly stop, in exchange for
// nothing, since the flag does not cover Enter anyway.
func TestNoKeyboardProtocolIsEnabled(t *testing.T) {
	for _, file := range []string{"extkeys.go", "../cli/cli.go"} {
		src, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		// The CSI > … u form is how the protocol is pushed. Its absence in a
		// string literal is what is being asserted; the prose above may name it.
		for _, lit := range []string{`"\x1b[>`, `"\x1b[<`} {
			if strings.Contains(string(src), lit) {
				t.Errorf("%s still writes %s to the terminal", file, lit)
			}
		}
	}
}
