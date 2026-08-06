package ui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/bubbles/viewport"
)

// openHelp shows the keybind/command reference in a narrow scrollable
// overlay.
func (m *App) openHelp() {
	w := min(56, m.w-6)
	h := max(6, m.h-7)
	m.textTitle = "help — j/k scroll, esc close"
	m.textVP = viewport.New(w-4, h-4)
	m.textVP.SetContent(m.helpContent(w - 6))
	m.overlay = overlayHelp
}

// helpContent builds the single-column help text.
func (m *App) helpContent(w int) string {
	var b strings.Builder
	section := func(title string) {
		b.WriteString("\n" + styPanelTitle.Render(title) + "\n")
	}
	entry := func(keys, help string) {
		if keys == "" {
			keys = "—"
		}
		b.WriteString(fmt.Sprintf("%s %s\n",
			styAccent.Render(padRight(truncate(keys, 15), 15)),
			styDim.Render(truncate(help, max(8, w-17)))))
	}

	groups := []string{"chat", "model", "agents", "session", "view", "scroll"}
	for _, g := range groups {
		section(g)
		for _, a := range actions {
			if a.Group == g {
				entry(m.keys.KeysFor(a.Name), a.Help)
			}
		}
	}

	section("slash commands")
	for _, c := range slashCmds {
		label := "/" + c.name
		if c.args != "" {
			label += " " + c.args
		}
		entry(label, c.help)
	}

	section("notes")
	for _, n := range []string{
		"click any pane to focus it",
		"dashboard: arrows pick a panel, shift+arrows",
		"  move it, enter edits the settings panel",
		"mark chat text with the mouse — it is",
		"  copied automatically",
		"keybinds: [keys] in config (/settings)",
		"shift+enter needs terminal support (README)",
	} {
		b.WriteString(styDim.Render(truncate(n, w)) + "\n")
	}
	return b.String()
}
