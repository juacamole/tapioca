package ui

import (
	"strings"

	"charm.land/lipgloss/v2"
)

// The welcome screen's wordmark: a block rendering of the name in the accent
// colour and nothing else, so the typography carries the identity rather than
// a mascot or an icon.
//
// It has to shrink, for two reasons that are properties of this app rather
// than preferences. The chat pane is two thirds of the screen, so the full
// mark's 53 columns do not fit an 80-column terminal — it would wrap into
// nonsense at exactly the size most people run. And the ascii glyph set
// promises to render in any terminal and font, which rules out block
// characters entirely, so that set gets a different drawing rather than a
// broken one.

// wordmarkFull is the ANSI Shadow lettering, at 53 columns.
var wordmarkFull = []string{
	`████████╗ █████╗ ██████╗ ██╗ ██████╗  ██████╗ █████╗ `,
	`╚══██╔══╝██╔══██╗██╔══██╗██║██╔═══██╗██╔════╝██╔══██╗`,
	`   ██║   ███████║██████╔╝██║██║   ██║██║     ███████║`,
	`   ██║   ██╔══██║██╔═══╝ ██║██║   ██║██║     ██╔══██║`,
	`   ██║   ██║  ██║██║     ██║╚██████╔╝╚██████╗██║  ██║`,
	`   ╚═╝   ╚═╝  ╚═╝╚═╝     ╚═╝ ╚═════╝  ╚═════╝╚═╝  ╚═╝`,
}

// wordmarkCompact is the same idea in half blocks, at 25 columns.
var wordmarkCompact = []string{
	`▀█▀ ▄▀█ █▀█ █ █▀█ █▀▀ ▄▀█`,
	` █  █▀█ █▀▀ █ █▄█ █▄▄ █▀█`,
}

// wordmarkPlain is for the ascii glyph set, which has no blocks to draw with.
var wordmarkPlain = []string{
	` _             _`,
	`| |_ __ _ _ __(_)___  __ __ _`,
	"|  _/ _` | '_ \\ / _ \\/ _/ _` |",
	` \__\__,_| .__/_\___/\__\__,_|`,
	`         |_|`,
}

// Wordmark modes. There is deliberately no "full": auto already draws the full
// mark whenever it fits, and a mode that drew it when it does not would only
// produce a wrapped mess.
const (
	WordmarkAuto    = "auto"    // the largest drawing that fits
	WordmarkCompact = "compact" // the small one, even where the large one fits
	WordmarkText    = "text"    // the name, no drawing
	WordmarkOff     = "off"     // nothing at all
)

// WordmarkModes lists the choices in the order the picker shows them.
var WordmarkModes = []struct{ Name, Desc string }{
	{WordmarkAuto, "largest that fits the pane (default)"},
	{WordmarkCompact, "always the small mark"},
	{WordmarkText, "just the name"},
	{WordmarkOff, "no wordmark"},
}

const defaultWordmark = WordmarkAuto

var wordmarkMode = defaultWordmark

// SetWordmark switches the mode, reporting the name that took effect. An
// unknown name falls back to the default rather than leaving the welcome
// screen blank.
func SetWordmark(name string) string {
	name = strings.ToLower(strings.TrimSpace(name))
	for _, m := range WordmarkModes {
		if m.Name == name {
			wordmarkMode = name
			return name
		}
	}
	wordmarkMode = defaultWordmark
	return defaultWordmark
}

// wordmark returns the drawing to use at w columns, or nil for none — the
// caller then falls back to the name as text, which always fits.
//
// Whatever the mode asks for still has to fit: a mark chosen by configuration
// wraps just as badly as one chosen automatically, and the person who set
// compact on a wide screen did not ask for a broken screen on a narrow one.
func wordmark(w int) []string {
	switch wordmarkMode {
	case WordmarkOff, WordmarkText:
		return nil
	}
	candidates := [][]string{wordmarkFull, wordmarkCompact}
	if wordmarkMode == WordmarkCompact {
		candidates = [][]string{wordmarkCompact}
	}
	if gl.plainText {
		candidates = [][]string{wordmarkPlain}
	}
	for _, c := range candidates {
		if widest(c) <= w {
			return c
		}
	}
	return nil
}

func widest(lines []string) int {
	w := 0
	for _, l := range lines {
		if n := lipgloss.Width(l); n > w {
			w = n
		}
	}
	return w
}

// renderWordmark draws the mark in the accent colour, or the name as plain
// text when nothing fits. mono has no colour to spend and styAppTitle is
// already bold there, so this needs no special case.
func renderWordmark(w int) []string {
	if wordmarkMode == WordmarkOff {
		return nil
	}
	mark := wordmark(w)
	if mark == nil {
		return []string{styAppTitle.Render("tapioca")}
	}
	out := make([]string, 0, len(mark))
	for _, l := range mark {
		out = append(out, styAppTitle.Render(l))
	}
	return out
}
