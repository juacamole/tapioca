package ui

import "github.com/charmbracelet/lipgloss"

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

// wordmark returns the largest drawing that fits w columns, or nil when even
// the smallest would not — the caller falls back to the name as text, which
// is always right and never wraps.
func wordmark(w int) []string {
	candidates := [][]string{wordmarkFull, wordmarkCompact}
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
