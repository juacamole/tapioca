package ui

import (
	"regexp"
	"strings"
	"testing"

	"tapioca/internal/provider"
)

// osc52 writes the user's clipboard; the CSI pair clears the screen and homes
// the cursor, which is what forging a permission prompt over the real one
// takes. c1csi/c1osc are the 8-bit forms textenc's Latin-1 fallback produces.
const (
	osc52  = "\x1b]52;c;cHduZWQ=\x07"
	clr    = "\x1b[2J\x1b[H"
	c1csi  = "\u009b2J"
	c1osc  = "\u009d52;c;cHduZWQ\u009c"
	nasty  = osc52 + clr + c1csi + c1osc
	nameIn = "srv__lookup"
)

// sgrRe matches the colour sequences lipgloss itself emits. Since v2 a style
// always carries them and the writer strips them for a destination that has no
// colour, so they are expected in a rendered string and are not what this is
// hunting for.
var sgrRe = regexp.MustCompile("\x1b\\[[0-9;:]*m")

// hasEscape reports whether anything a terminal would act on survived, once
// our own colouring is discounted. Anything else — a clipboard write, a screen
// clear, a C1 introducer — is attacker-controlled and must not be there.
func hasEscape(s string) bool {
	s = sgrRe.ReplaceAllString(s, "")
	if strings.ContainsAny(s, "\x1b\x07") {
		return true
	}
	for _, r := range s {
		if r >= 0x80 && r <= 0x9f {
			return true
		}
	}
	return false
}

// A tool name is attacker-controlled: it comes from an MCP server, or straight
// from a provider response carrying a crafted tool_use block.
func TestToolNameEscapesNeverReachTheScreen(t *testing.T) {
	bl := provider.Block{Type: "tool_use", Name: nameIn + nasty, Input: []byte(`{"a":1}`)}
	for _, verbose := range []bool{false, true} {
		out := renderToolUse(&bl, 80, verbose)
		if hasEscape(out) {
			t.Fatalf("renderToolUse(verbose=%v) leaked an escape: %q", verbose, out)
		}
		if !strings.Contains(out, nameIn) {
			t.Fatalf("the readable part of the name was lost: %q", out)
		}
	}
	res := provider.Block{Type: "tool_result", Name: nameIn + nasty, Content: "ok"}
	if out := renderToolResult(&res, 80, false); hasEscape(out) {
		t.Fatalf("renderToolResult leaked an escape: %q", out)
	}
}

// Diff rows are file contents, so a checked-in escape reaches the terminal
// through a file nobody chose to display.
func TestDiffEscapesNeverReachTheScreen(t *testing.T) {
	display := "updated main.go (+1 -0)\n  1 + " + nasty + "evil"
	out := renderChange(display, 80)
	if hasEscape(out) {
		t.Fatalf("renderChange leaked an escape: %q", out)
	}
	if !strings.Contains(out, "evil") {
		t.Fatalf("the diff content itself was lost: %q", out)
	}
}

// xterm acts on 8-bit C1 by default, and textenc manufactures those code
// points from bytes 0x80-0x9f in its Latin-1 fallback.
func TestSanitizeStripsC1Controls(t *testing.T) {
	got := sanitizeText("a" + c1csi + "b" + c1osc + "c")
	if hasEscape(got) {
		t.Fatalf("C1 control survived: %q", got)
	}
	for _, want := range []string{"a", "b", "c"} {
		if !strings.Contains(got, want) {
			t.Fatalf("readable text was lost: %q", got)
		}
	}
}

// A newline in a label would draw extra rows and push real content off screen.
func TestSanitizeLabelIsOneLine(t *testing.T) {
	if got := sanitizeLabel("a\nb\nc"); strings.Contains(got, "\n") {
		t.Fatalf("label spans lines: %q", got)
	}
}

// Ordinary text must come through untouched, or the fix costs every render.
func TestSanitizeLeavesOrdinaryTextAlone(t *testing.T) {
	for _, s := range []string{"hello world", "func main() {}\n x := 1", "café ☕ 日本語", ""} {
		if got := sanitizeText(s); got != s {
			t.Errorf("sanitizeText(%q) = %q", s, got)
		}
	}
}

// Sanitizing at each render site meant one missed site was an escape on
// screen, and the audit found eleven. These check the choke points instead.
func TestFlashIsSanitized(t *testing.T) {
	m := &App{}
	m.setFlash("provider said: "+nasty, true)
	if hasEscape(m.flash) {
		t.Fatalf("flash kept an escape: %q", m.flash)
	}
}

func TestPickerItemsAreSanitized(t *testing.T) {
	p := newPicker(pickTheme, "t", []pickerItem{
		{label: "session " + nasty, desc: "resumed " + nasty, value: "x"},
	})
	if hasEscape(p.items[0].label) || hasEscape(p.items[0].desc) {
		t.Fatalf("picker kept an escape: %+v", p.items[0])
	}
}

func TestTextOverlayIsSanitized(t *testing.T) {
	m := &App{w: 80, h: 24}
	m.openTextOverlay("diff", "a "+nasty+" b")
	if hasEscape(m.textVP.View()) {
		t.Fatalf("overlay kept an escape: %q", m.textVP.View())
	}
}

// The classes above match code points, so a lone 0x9b byte in a string that is
// not valid UTF-8 decoded to U+FFFD and was never seen — while an 8-bit
// terminal reads it as CSI. Raw bash output reaches the screen without going
// through textenc.Decode, so this arrives as bytes, not runes.
func TestSanitizeStripsRawC1Bytes(t *testing.T) {
	raw := "out " + string([]byte{0x9b}) + "31mred" + string([]byte{0x9d}) + "52;c;x"
	got := sanitizeText(raw)
	for i := 0; i < len(got); i++ {
		if got[i] >= 0x80 && got[i] <= 0x9f {
			t.Fatalf("raw C1 byte survived at %d: %q", i, got)
		}
	}
	if !strings.Contains(got, "out ") || !strings.Contains(got, "red") {
		t.Fatalf("readable text was lost: %q", got)
	}
}

// Valid multi-byte UTF-8 must survive: 0x9b is a legitimate continuation byte.
func TestSanitizeKeepsMultibyteText(t *testing.T) {
	for _, s := range []string{"café ☕ 日本語", "→ ± ½", "🇩🇪 flag"} {
		if got := sanitizeText(s); got != s {
			t.Errorf("sanitizeText(%q) = %q", s, got)
		}
	}
}
