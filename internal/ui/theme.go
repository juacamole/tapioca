package ui

import (
	"sort"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// palette is one theme's colors as light/dark pairs. mono themes ignore the
// colors entirely and lean on bold/faint/reverse, for terminals with a fixed
// palette or users who want no color at all.
type palette struct {
	desc                                                  string
	mono                                                  bool
	accent, dim, user, err, ok, warn, border, think, tool lipgloss.AdaptiveColor
	codeBg                                                lipgloss.AdaptiveColor
	agents                                                []lipgloss.AdaptiveColor
}

const defaultTheme = "taro"

// themes are the built-in palettes. contrast uses the Okabe-Ito colorblind-safe
// set, which stays distinguishable under deuteranopia and protanopia — the
// usual red/green pairing does not.
var themes = map[string]palette{
	"taro": {
		desc:   "violet accent, cool neutrals (default)",
		accent: lipgloss.AdaptiveColor{Light: "#6C4FD8", Dark: "#A78BFA"},
		dim:    lipgloss.AdaptiveColor{Light: "#8B8B98", Dark: "#6A6A78"},
		user:   lipgloss.AdaptiveColor{Light: "#1F8A5D", Dark: "#6EE7A8"},
		err:    lipgloss.AdaptiveColor{Light: "#D03050", Dark: "#FB7185"},
		ok:     lipgloss.AdaptiveColor{Light: "#1F8A5D", Dark: "#6EE7A8"},
		warn:   lipgloss.AdaptiveColor{Light: "#B45309", Dark: "#FBBF24"},
		border: lipgloss.AdaptiveColor{Light: "#D9D9E3", Dark: "#33333E"},
		think:  lipgloss.AdaptiveColor{Light: "#8E44AD", Dark: "#D8B4FE"},
		tool:   lipgloss.AdaptiveColor{Light: "#1D6FB8", Dark: "#7CC7FF"},
		codeBg: lipgloss.AdaptiveColor{Light: "#F1F1F6", Dark: "#232330"},
		agents: []lipgloss.AdaptiveColor{
			{Light: "#6C4FD8", Dark: "#A78BFA"},
			{Light: "#1D6FB8", Dark: "#7CC7FF"},
			{Light: "#0F766E", Dark: "#5EEAD4"},
			{Light: "#1F8A5D", Dark: "#6EE7A8"},
			{Light: "#BE3B78", Dark: "#F9A8D4"},
			{Light: "#B45309", Dark: "#FBBF24"},
		},
	},
	"contrast": {
		desc:   "colorblind-safe, high contrast",
		accent: lipgloss.AdaptiveColor{Light: "#0072B2", Dark: "#56B4E9"},
		dim:    lipgloss.AdaptiveColor{Light: "#6B6B6B", Dark: "#9A9A9A"},
		user:   lipgloss.AdaptiveColor{Light: "#006B54", Dark: "#009E73"},
		err:    lipgloss.AdaptiveColor{Light: "#B33A00", Dark: "#D55E00"},
		ok:     lipgloss.AdaptiveColor{Light: "#006B54", Dark: "#009E73"},
		warn:   lipgloss.AdaptiveColor{Light: "#9A6A00", Dark: "#E69F00"},
		border: lipgloss.AdaptiveColor{Light: "#9A9A9A", Dark: "#5A5A5A"},
		think:  lipgloss.AdaptiveColor{Light: "#8A4B72", Dark: "#CC79A7"},
		tool:   lipgloss.AdaptiveColor{Light: "#0072B2", Dark: "#56B4E9"},
		codeBg: lipgloss.AdaptiveColor{Light: "#EDEDED", Dark: "#2A2A2A"},
		agents: []lipgloss.AdaptiveColor{
			{Light: "#0072B2", Dark: "#56B4E9"},
			{Light: "#B33A00", Dark: "#D55E00"},
			{Light: "#006B54", Dark: "#009E73"},
			{Light: "#9A6A00", Dark: "#E69F00"},
			{Light: "#8A4B72", Dark: "#CC79A7"},
			{Light: "#5A5A5A", Dark: "#BBBBBB"},
		},
	},
	"mono": {
		desc: "no color; bold and dim only",
		mono: true,
	},
}

func themeNames() []string {
	names := make([]string, 0, len(themes))
	for n := range themes {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// Active palette and the styles derived from it. Every style is a package var
// so a theme switch can rebuild them in place while the program runs.
var (
	active = themes[defaultTheme]

	colAccent = active.accent
	colBorder = active.border

	styAppTitle   lipgloss.Style
	styDim        lipgloss.Style
	styErr        lipgloss.Style
	styOK         lipgloss.Style
	styWarn       lipgloss.Style
	styUser       lipgloss.Style
	styThink      lipgloss.Style
	styTool       lipgloss.Style
	styCode       lipgloss.Style
	styFlash      lipgloss.Style
	styPanelTitle lipgloss.Style
	styAccent     lipgloss.Style
	styTabActive  lipgloss.Style
	styTabIdle    lipgloss.Style
	stySelected   lipgloss.Style
)

func init() { applyPalette(active) }

// SetTheme switches to a named theme with optional per-key overrides, and
// reports the name that ended up active. An unknown name falls back to the
// default rather than leaving the UI unstyled.
func SetTheme(name string, overrides map[string]string) string {
	name = strings.ToLower(strings.TrimSpace(name))
	p, ok := themes[name]
	if !ok {
		name, p = defaultTheme, themes[defaultTheme]
	}
	for key, val := range overrides {
		applyOverride(&p, key, val)
	}
	active = p
	applyPalette(p)
	return name
}

// parseColor accepts "#hex" for both backgrounds, or "#light/#dark" to differ
// between them. Slash separates the pair so comma stays free for lists.
func parseColor(val string) lipgloss.AdaptiveColor {
	light, dark, ok := strings.Cut(val, "/")
	if !ok {
		dark = light
	}
	return lipgloss.AdaptiveColor{
		Light: strings.TrimSpace(light),
		Dark:  strings.TrimSpace(dark),
	}
}

// applyOverride sets one palette entry from config. Unknown keys are ignored:
// a typo should not blank a color.
func applyOverride(p *palette, key, val string) {
	val = strings.TrimSpace(val)
	if val == "" {
		return
	}
	if strings.ToLower(strings.TrimSpace(key)) == "agents" {
		var cols []lipgloss.AdaptiveColor
		for _, part := range strings.Split(val, ",") {
			if part = strings.TrimSpace(part); part != "" {
				cols = append(cols, parseColor(part))
			}
		}
		if len(cols) > 0 {
			p.agents, p.mono = cols, false
		}
		return
	}
	c := parseColor(val)
	switch strings.ToLower(strings.TrimSpace(key)) {
	case "accent":
		p.accent = c
	case "dim":
		p.dim = c
	case "user":
		p.user = c
	case "error", "err":
		p.err = c
	case "ok", "success":
		p.ok = c
	case "warn", "warning":
		p.warn = c
	case "border":
		p.border = c
	case "think", "thinking":
		p.think = c
	case "tool":
		p.tool = c
	case "code_bg", "code":
		p.codeBg = c
	default:
		return // a typo must not blank a color
	}
	// Naming a color means color is wanted, so it lifts a mono base.
	p.mono = false
}

func applyPalette(p palette) {
	colAccent, colBorder = p.accent, p.border

	fg := func(c lipgloss.AdaptiveColor) lipgloss.Style {
		if p.mono {
			return lipgloss.NewStyle()
		}
		return lipgloss.NewStyle().Foreground(c)
	}

	styAppTitle = fg(p.accent).Bold(true)
	styDim = fg(p.dim)
	styErr = fg(p.err)
	styOK = fg(p.ok)
	styWarn = fg(p.warn)
	styUser = fg(p.user).Bold(true)
	styThink = fg(p.think).Italic(true)
	styTool = fg(p.tool)
	styFlash = fg(p.accent).Bold(true)
	styPanelTitle = fg(p.accent).Bold(true)
	styAccent = fg(p.accent)
	styTabActive = fg(p.accent).Bold(true).Underline(true)
	styTabIdle = fg(p.dim)
	stySelected = fg(p.accent).Bold(true).Reverse(true)

	if p.mono {
		// Without color these have to carry their meaning some other way.
		styDim = lipgloss.NewStyle().Faint(true)
		styErr = lipgloss.NewStyle().Bold(true)
		styWarn = lipgloss.NewStyle().Bold(true)
		styThink = lipgloss.NewStyle().Faint(true).Italic(true)
		styTabIdle = lipgloss.NewStyle().Faint(true)
		styCode = lipgloss.NewStyle().Reverse(true)
	} else {
		styCode = lipgloss.NewStyle().Background(p.codeBg)
	}
}

// borderStyle draws a panel border. With color, focus is the accent color;
// mono has no color to spend, so it switches to the glyph set's heavier
// border — taking it from the set keeps ascii mode free of box-drawing.
func borderStyle(focused bool) lipgloss.Style {
	if active.mono {
		b := gl.border
		if focused {
			b = gl.focusBorder
		}
		return lipgloss.NewStyle().Border(b)
	}
	c := colBorder
	if focused {
		c = colAccent
	}
	return lipgloss.NewStyle().Border(gl.border).BorderForeground(c)
}

// applyTheme switches theme, persists it and repaints. Cached renders hold
// the old escape codes, so the chat cache has to go with it.
func (m *App) applyTheme(name string) tea.Cmd {
	m.cfg.Theme = SetTheme(name, m.cfg.Colors)
	m.saveCfg()
	m.repaint()
	m.setFlash("theme "+m.cfg.Theme, false)
	return m.flashCmd()
}

func (m *App) applyGlyphs(name string) tea.Cmd {
	m.cfg.Glyphs = SetGlyphs(name)
	m.spin.Spinner = gl.spinner
	m.saveCfg()
	m.repaint()
	m.setFlash("glyphs "+m.cfg.Glyphs, false)
	return m.flashCmd()
}

// repaint drops render caches so restyled output replaces the old.
func (m *App) repaint() {
	msgCache = map[string]string{}
	mdCache = map[string]string{}
	m.refreshChat(true)
}

func (m *App) openThemePicker() {
	items := make([]pickerItem, 0, len(themes))
	for _, n := range themeNames() {
		items = append(items, pickerItem{label: n, desc: themes[n].desc, value: n})
	}
	m.pick = newPicker(pickTheme, "theme", items)
	m.overlay = overlayPicker
}

func (m *App) applyWordmark(name string) tea.Cmd {
	m.cfg.Wordmark = SetWordmark(name)
	m.saveCfg()
	m.repaint()
	m.setFlash("wordmark "+m.cfg.Wordmark, false)
	return m.flashCmd()
}

func (m *App) openWordmarkPicker() {
	items := make([]pickerItem, 0, len(WordmarkModes))
	for _, w := range WordmarkModes {
		items = append(items, pickerItem{label: w.Name, desc: w.Desc, value: w.Name})
	}
	m.pick = newPicker(pickWordmark, "wordmark", items)
	m.overlay = overlayPicker
}

func (m *App) openGlyphsPicker() {
	items := make([]pickerItem, 0, len(glyphSets))
	for _, n := range glyphNames() {
		items = append(items, pickerItem{label: n, desc: glyphSets[n].desc, value: n})
	}
	m.pick = newPicker(pickGlyphs, "glyph set", items)
	m.overlay = overlayPicker
}

func agentColor(id int) lipgloss.TerminalColor {
	if active.mono || len(active.agents) == 0 {
		return lipgloss.NoColor{}
	}
	return active.agents[(id-1+len(active.agents))%len(active.agents)]
}
