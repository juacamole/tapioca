package ui

import (
	"sort"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/lipgloss"
)

// A terminal owns its font, so "font" here means the glyph set the UI draws
// with. ascii renders identically everywhere (tmux, CI logs, serial consoles,
// fonts without box drawing); unicode is the default; nerd adds icons and
// needs a patched font installed.
type glyphSet struct {
	desc string

	// plainText marks a set that may use nothing outside ASCII, so drawings
	// made of block characters have to be swapped rather than merely styled.
	plainText bool

	sep      string // padded separator between fields, e.g. " · "
	sepTight string // unpadded separator, e.g. "1·agent"
	ellipsis string
	dot      string // agent status
	check    string // picker selection
	caret    string // picker cursor
	bar      string // chat quote marker
	line     string // horizontal rule

	gaugeFull  string
	gaugeEmpty string
	spark      []rune

	border      lipgloss.Border
	focusBorder lipgloss.Border
	spinner     spinner.Spinner

	toolOK, toolErr               string // tool call outcome markers
	todoDone, todoDoing, todoWait string
}

const defaultGlyphs = "unicode"

// asciiFocusBorder marks focus with a doubled edge, since ASCII has no
// heavy-line box drawing to switch to.
var asciiFocusBorder = lipgloss.Border{
	Top: "=", Bottom: "=", Left: "|", Right: "|",
	TopLeft: "+", TopRight: "+", BottomLeft: "+", BottomRight: "+",
}

var glyphSets = map[string]glyphSet{
	"unicode": {
		desc:        "box drawing and symbols (default)",
		sep:         " · ",
		sepTight:    "·",
		ellipsis:    "…",
		dot:         "●",
		check:       "✓",
		caret:       "›",
		bar:         "▌",
		line:        "─",
		gaugeFull:   "█",
		gaugeEmpty:  "░",
		spark:       []rune("▁▂▃▄▅▆▇█"),
		border:      lipgloss.RoundedBorder(),
		focusBorder: lipgloss.ThickBorder(),
		spinner:     spinner.MiniDot,
		toolOK:      "+",
		toolErr:     "x",
		todoDone:    "x",
		todoDoing:   ">",
		todoWait:    "·",
	},
	"ascii": {
		desc:        "pure ASCII; renders in any terminal",
		plainText:   true,
		sep:         " | ",
		sepTight:    "|",
		ellipsis:    "...",
		dot:         "*",
		check:       "x",
		caret:       ">",
		bar:         "|",
		line:        "-",
		gaugeFull:   "#",
		gaugeEmpty:  ".",
		spark:       []rune("._-=+*%#"),
		border:      lipgloss.ASCIIBorder(),
		focusBorder: asciiFocusBorder,
		spinner: spinner.Spinner{
			Frames: []string{"-", "\\", "|", "/"},
			FPS:    time.Second / 10,
		},
		toolOK:    "+",
		toolErr:   "x",
		todoDone:  "x",
		todoDoing: ">",
		todoWait:  "-",
	},
	"nerd": {
		desc:        "Nerd Font icons; needs a patched font",
		sep:         " · ",
		sepTight:    "·",
		ellipsis:    "…",
		dot:         "", // circle
		check:       "", // check
		caret:       "", // chevron-right
		bar:         "▌",
		line:        "─",
		gaugeFull:   "█",
		gaugeEmpty:  "░",
		spark:       []rune("▁▂▃▄▅▆▇█"),
		border:      lipgloss.RoundedBorder(),
		focusBorder: lipgloss.ThickBorder(),
		spinner:     spinner.Dot,
		toolOK:      "", // check
		toolErr:     "", // times
		todoDone:    "",
		todoDoing:   "",
		todoWait:    "", // circle-o
	},
}

// gl is the active glyph set; every drawing site reads through it.
var gl = glyphSets[defaultGlyphs]

func glyphNames() []string {
	names := make([]string, 0, len(glyphSets))
	for n := range glyphSets {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// SetGlyphs switches the glyph set, reporting the name that took effect.
func SetGlyphs(name string) string {
	name = strings.ToLower(strings.TrimSpace(name))
	g, ok := glyphSets[name]
	if !ok {
		name, g = defaultGlyphs, glyphSets[defaultGlyphs]
	}
	gl = g
	return name
}
