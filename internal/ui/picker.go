package ui

import (
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// pickerKind identifies what a picker selection means.
type pickerKind int

const (
	pickNone pickerKind = iota
	pickModel
	pickSession
	pickPanels
	pickCheckpoint
	pickTheme
	pickGlyphs
	pickWordmark
	pickEffort
)

type pickerItem struct {
	label  string
	desc   string
	value  string
	search string // extra text matched by the filter (e.g. session contents)
}

// picker is a modal list with typed filtering. For pickPanels it acts as a
// multi-toggle where enter/space flips a panel on or off.
type picker struct {
	kind   pickerKind
	title  string
	items  []pickerItem
	sel    int
	filter string
}

// newPicker sanitizes every item once here rather than at each call site:
// pickers list model-generated session titles, checkpoint labels built from
// tool arguments, and file paths, and one missed site is an escape on screen.
func newPicker(kind pickerKind, title string, items []pickerItem) picker {
	for i := range items {
		items[i].label = sanitizeLabel(items[i].label)
		items[i].desc = sanitizeLabel(items[i].desc)
	}
	return picker{kind: kind, title: title, items: items}
}

func (p *picker) visible() []pickerItem {
	if p.filter == "" {
		return p.items
	}
	f := strings.ToLower(p.filter)
	var out []pickerItem
	for _, it := range p.items {
		if strings.Contains(strings.ToLower(it.label), f) ||
			strings.Contains(strings.ToLower(it.desc), f) ||
			(it.search != "" && strings.Contains(it.search, f)) {
			out = append(out, it)
		}
	}
	return out
}

// selected returns the currently highlighted item, or nil.
func (p *picker) selected() *pickerItem {
	vis := p.visible()
	if len(vis) == 0 {
		return nil
	}
	if p.sel >= len(vis) {
		p.sel = len(vis) - 1
	}
	if p.sel < 0 {
		p.sel = 0
	}
	return &vis[p.sel]
}

// handleKey processes a key; it returns (chosen, closed).
func (p *picker) handleKey(msg tea.KeyMsg) (*pickerItem, bool) {
	switch msg.String() {
	case "esc":
		return nil, true
	case "up", "ctrl+k":
		p.sel--
		if p.sel < 0 {
			p.sel = max(0, len(p.visible())-1)
		}
	case "down", "ctrl+j":
		p.sel++
		if p.sel >= len(p.visible()) {
			p.sel = 0
		}
	case "enter", " ":
		if msg.String() == " " && p.kind != pickPanels {
			p.filter += " "
			break
		}
		if it := p.selected(); it != nil {
			return it, p.kind != pickPanels // panel picker stays open
		}
	case "backspace":
		if len(p.filter) > 0 {
			r := []rune(p.filter)
			p.filter = string(r[:len(r)-1])
		}
	default:
		if msg.Type == tea.KeyRunes && !msg.Alt {
			p.filter += string(msg.Runes)
			p.sel = 0
		}
	}
	return nil, false
}

// view renders the modal centered in (w, h).
func (p *picker) view(w, h int, checked func(string) bool) string {
	boxW := min(64, max(30, w-8))
	innerW := boxW - 2
	contentW := innerW - 2 // horizontal padding inside the border
	vis := p.visible()

	var b strings.Builder
	b.WriteString(styPanelTitle.Render(truncate(p.title, contentW)) + "\n")
	filter := p.filter
	if filter == "" {
		filter = styDim.Render("type to filter…")
	}
	b.WriteString(styDim.Render("filter: ") + filter + "\n\n")

	maxRows := max(3, h-10)
	start := 0
	if p.sel >= maxRows {
		start = p.sel - maxRows + 1
	}
	end := min(len(vis), start+maxRows)
	if len(vis) == 0 {
		b.WriteString(styDim.Render("nothing matches"))
	}
	for i := start; i < end; i++ {
		it := vis[i]
		mark := ""
		if p.kind == pickPanels && checked != nil {
			if checked(it.value) {
				mark = styOK.Render(gl.check + " ")
			} else {
				mark = styDim.Render(gl.todoWait + " ")
			}
		}
		if i == p.sel {
			plain := gl.caret + " " + it.label
			if it.desc != "" {
				plain += "  " + it.desc
			}
			b.WriteString(stySelected.Render(padRight(truncate(plain, contentW), contentW)) + "\n")
			continue
		}
		line := "  " + mark + it.label
		if it.desc != "" {
			line += styDim.Render("  " + it.desc)
		}
		b.WriteString(lipgloss.NewStyle().MaxWidth(contentW).Render(line) + "\n")
	}
	if !zenMode {
		hint := "up/down move" + gl.sep + "enter select" + gl.sep + "esc close"
		if p.kind == pickPanels {
			hint = "enter toggle" + gl.sep + "left/right reorder" + gl.sep + "esc close"
		}
		b.WriteString("\n" + styDim.Render(hint))
	}

	box := borderStyle(true).Width(innerW).Padding(0, 1).Render(b.String())
	return lipgloss.Place(w, h, lipgloss.Center, lipgloss.Center, box)
}
