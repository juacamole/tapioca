package ui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
)

// A wordmark that does not fit is worse than none: it wraps mid-letter and the
// welcome screen becomes noise. Every drawing must therefore fit the width it
// is chosen for, and its rows must be equal, or the letters shear.
func TestWordmarkDrawingsAreWellFormed(t *testing.T) {
	for name, mark := range map[string][]string{
		"full":    wordmarkFull,
		"compact": wordmarkCompact,
		"plain":   wordmarkPlain,
	} {
		if len(mark) == 0 {
			t.Errorf("%s is empty", name)
			continue
		}
		w := lipgloss.Width(mark[0])
		for i, line := range mark {
			if got := lipgloss.Width(line); got != w {
				// plain is figlet output, which is ragged on the right by
				// design; only the block drawings have to be rectangular.
				if name != "plain" {
					t.Errorf("%s row %d is %d wide, row 0 is %d — letters will shear", name, i, got, w)
				}
			}
			if strings.ContainsAny(line, "\n\t") {
				t.Errorf("%s row %d contains a newline or tab", name, i)
			}
		}
	}
}

// The ascii glyph set promises to render in any terminal and font, so it may
// not fall back to block characters.
func TestPlainDrawingIsActuallyAscii(t *testing.T) {
	for i, line := range wordmarkPlain {
		for _, r := range line {
			if r > 0x7e || r < 0x20 {
				t.Fatalf("row %d contains %q (U+%04X), which is not ascii", i, r, r)
			}
		}
	}
}

func TestWordmarkPicksTheLargestThatFits(t *testing.T) {
	saved := gl
	defer func() { gl = saved }()
	gl = glyphSets["unicode"]

	full := lipgloss.Width(wordmarkFull[0])
	compact := lipgloss.Width(wordmarkCompact[0])

	if got := wordmark(full); len(got) != len(wordmarkFull) {
		t.Errorf("at exactly the full width, got %d rows, want the full mark", len(got))
	}
	if got := wordmark(full - 1); len(got) != len(wordmarkCompact) {
		t.Errorf("one column short of full, got %d rows, want the compact mark", len(got))
	}
	if got := wordmark(compact); len(got) != len(wordmarkCompact) {
		t.Errorf("at exactly the compact width, got %d rows, want the compact mark", len(got))
	}
	if got := wordmark(compact - 1); got != nil {
		t.Errorf("one column short of compact, got a mark %d wide; want nil so the caller uses plain text", widest(got))
	}
}

// Whatever is chosen must fit, at every width, in both glyph sets. This is the
// property that actually matters; the cases above are just its edges.
func TestChosenWordmarkAlwaysFits(t *testing.T) {
	saved := gl
	defer func() { gl = saved }()
	for _, set := range []string{"unicode", "ascii", "nerd"} {
		gl = glyphSets[set]
		for w := 1; w <= 120; w++ {
			mark := wordmark(w)
			if mark == nil {
				continue
			}
			if got := widest(mark); got > w {
				t.Fatalf("%s at width %d chose a mark %d wide", set, w, got)
			}
			if gl.plainText {
				for _, line := range mark {
					for _, r := range line {
						if r > 0x7e {
							t.Fatalf("ascii set at width %d chose a mark containing %q", w, r)
						}
					}
				}
			}
		}
	}
}

// The welcome screen must render at any width without panicking or emitting a
// line wider than the pane.
func TestWelcomeTextFitsItsWidth(t *testing.T) {
	saved, savedZen := gl, zenMode
	defer func() { gl, zenMode = saved, savedZen }()
	gl = glyphSets["unicode"]

	for _, zen := range []bool{false, true} {
		zenMode = zen
		for _, w := range []int{10, 20, 25, 26, 40, 53, 60, 100} {
			for _, line := range strings.Split(welcomeText(w), "\n") {
				// Hint lines are wrapped by the caller, so only the mark rows
				// are checked here — they are the ones that cannot wrap.
				if strings.ContainsAny(line, "█▀▄╗╝║═") && lipgloss.Width(line) > w {
					t.Errorf("zen=%v width=%d: mark row is %d wide", zen, w, lipgloss.Width(line))
				}
			}
		}
	}
}

// The mode is configuration, but a mark that does not fit wraps just as badly
// whoever chose it — so compact on a narrow pane must still degrade.
func TestWordmarkModesRespectWidth(t *testing.T) {
	savedGl, savedMode := gl, wordmarkMode
	defer func() { gl, wordmarkMode = savedGl, savedMode }()
	gl = glyphSets["unicode"]

	SetWordmark(WordmarkCompact)
	if got := wordmark(200); len(got) != len(wordmarkCompact) {
		t.Error("compact mode drew something other than the compact mark on a wide pane")
	}
	if got := wordmark(5); got != nil {
		t.Errorf("compact mode drew a %d-wide mark into 5 columns", widest(got))
	}

	SetWordmark(WordmarkAuto)
	if got := wordmark(200); len(got) != len(wordmarkFull) {
		t.Error("auto did not choose the full mark on a wide pane")
	}

	for _, mode := range []string{WordmarkText, WordmarkOff} {
		SetWordmark(mode)
		if got := wordmark(200); got != nil {
			t.Errorf("%s drew a mark", mode)
		}
	}
}

// off means nothing; text means the name. The difference has to be visible.
func TestOffAndTextDiffer(t *testing.T) {
	savedGl, savedMode, savedZen := gl, wordmarkMode, zenMode
	defer func() { gl, wordmarkMode, zenMode = savedGl, savedMode, savedZen }()
	gl, zenMode = glyphSets["unicode"], false

	SetWordmark(WordmarkText)
	if got := renderWordmark(100); len(got) != 1 || !strings.Contains(got[0], "tapioca") {
		t.Errorf("text mode rendered %q, want the name", got)
	}
	SetWordmark(WordmarkOff)
	if got := renderWordmark(100); len(got) != 0 {
		t.Errorf("off mode rendered %q, want nothing", got)
	}
	// And the welcome screen must not open with a stray blank line.
	if first := strings.SplitN(welcomeText(100), "\n", 2)[0]; strings.TrimSpace(first) == "" {
		t.Error("off mode left a leading blank line on the welcome screen")
	}
}

// An unknown mode in a hand-edited config must not blank the screen.
func TestUnknownModeFallsBackToAuto(t *testing.T) {
	saved := wordmarkMode
	defer func() { wordmarkMode = saved }()
	for _, bad := range []string{"", "enormous", "OFF ", "nonsense"} {
		got := SetWordmark(bad)
		if bad == "OFF " {
			// trimmed and lowercased, so this one is valid
			if got != WordmarkOff {
				t.Errorf("SetWordmark(%q) = %q, want off", bad, got)
			}
			continue
		}
		if got != WordmarkAuto {
			t.Errorf("SetWordmark(%q) = %q, want auto", bad, got)
		}
	}
}
