package ui

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/charmbracelet/bubbles/textarea"
	tea "github.com/charmbracelet/bubbletea"
)

// Mouse text selection in the chat viewport: drag with the left button to
// mark text; on release the marked region is copied to the clipboard.
// While a region is marked its lines render as plain reversed text.

var ansiRe = regexp.MustCompile("\x1b\\[[0-9;]*m")

func stripAnsi(s string) string { return ansiRe.ReplaceAllString(s, "") }

type selPoint struct{ line, col int }

func (p selPoint) before(q selPoint) bool {
	return p.line < q.line || (p.line == q.line && p.col <= q.col)
}

// mouseToChat maps screen coordinates to (content line, column) inside the
// chat viewport. ok is false outside the transcript area.
func (m *App) mouseToChat(x, y int) (selPoint, bool) {
	dashW, dashH := m.dashDims()
	chatLeft := 0
	if m.cfg.Dashboard.Position == "left" {
		chatLeft = dashW
	}
	chatTop := 3 // title + tabs + top border
	if m.cfg.Dashboard.Position == "top" {
		chatTop += dashH
	}
	innerX := x - chatLeft - 1
	innerY := y - chatTop
	if innerX < 0 || innerY < 0 || innerY >= m.vp.Height || innerX >= m.vp.Width {
		return selPoint{}, false
	}
	line := m.vp.YOffset + innerY
	if line < 0 || line >= len(m.chatPlain) {
		return selPoint{}, false
	}
	col := innerX
	if n := len([]rune(m.chatPlain[line])); col > n {
		col = n
	}
	return selPoint{line: line, col: col}, true
}

type region int

const (
	regionNone region = iota
	regionChat
	regionInput
	regionDash
)

// regionAt maps screen coordinates to a UI region; for regionDash it also
// returns the clicked panel index.
func (m *App) regionAt(x, y int) (region, int) {
	if !m.ready {
		return regionNone, 0
	}
	bodyTop := 2
	bodyH := m.h - 3
	dashW, dashH := m.dashDims()
	pos := m.cfg.Dashboard.Position

	// Dashboard rectangle.
	if dashW > 0 || dashH > 0 {
		dx, dy, dw, dh := 0, bodyTop, 0, 0
		switch {
		case dashW > 0 && pos == "left":
			dw, dh = dashW, bodyH
		case dashW > 0:
			dx, dw, dh = m.w-dashW, dashW, bodyH
		case pos == "top":
			dw, dh = m.w, dashH
		default: // bottom
			dy, dw, dh = bodyTop+bodyH-dashH, m.w, dashH
		}
		if x >= dx && x < dx+dw && y >= dy && y < dy+dh {
			var defs []*panelDef
			var sizes []int
			var vertical bool
			if dashW > 0 {
				defs, sizes, vertical = m.dashLayout(dashW, bodyH)
			} else {
				defs, sizes, vertical = m.dashLayout(m.w, dashH)
			}
			offset := y - dy
			if !vertical {
				offset = x - dx
			}
			idx := 0
			for i, s := range sizes {
				if offset < s {
					idx = i
					break
				}
				offset -= s
				idx = i
			}
			_ = defs
			return regionDash, idx
		}
	}

	// Chat box.
	chatLeft := 0
	if pos == "left" {
		chatLeft = dashW
	}
	chatTop := bodyTop
	if pos == "top" {
		chatTop += dashH
	}
	chatH := bodyH - dashH
	chatW := m.w - dashW
	if x < chatLeft || x >= chatLeft+chatW {
		return regionNone, 0
	}
	vpTop := chatTop + 1
	switch {
	case y >= vpTop && y < vpTop+m.vp.Height:
		return regionChat, 0
	case y >= vpTop+m.vp.Height+1 && y < chatTop+chatH-1:
		return regionInput, 0
	}
	return regionNone, 0
}

// handleChatMouse processes clicks (focus follows the mouse) and selection
// drags; it reports whether the event was consumed and any follow-up cmd.
func (m *App) handleChatMouse(msg tea.MouseMsg) (bool, tea.Cmd) {
	if m.overlay != overlayNone {
		return false, nil
	}
	switch msg.Action {
	case tea.MouseActionPress:
		if msg.Button != tea.MouseButtonLeft {
			return false, nil
		}
		r, idx := m.regionAt(msg.X, msg.Y)
		switch r {
		case regionInput:
			m.focus = focusInput
			m.clearSelection()
			return true, tea.Batch(m.ta.Focus(), textarea.Blink)
		case regionDash:
			m.focus = focusDash
			m.dashEditing = false
			m.dashPanelSel = idx
			m.ta.Blur()
			m.clearSelection()
			return true, nil
		case regionChat:
			// Clicking the transcript goes straight to write mode; a drag
			// still selects text (selection is independent of focus).
			m.focus = focusInput
			cmd := tea.Batch(m.ta.Focus(), textarea.Blink)
			m.clearSelection() // drop any previous (still highlighted) mark
			p, ok := m.mouseToChat(msg.X, msg.Y)
			if !ok {
				return true, cmd
			}
			m.selActive = true
			m.selStart, m.selEnd = p, p
			m.applySelection()
			return true, cmd
		}
		return false, nil
	case tea.MouseActionMotion:
		if !m.selActive {
			return false, nil
		}
		if p, ok := m.mouseToChat(msg.X, msg.Y); ok {
			m.selEnd = p
			m.applySelection()
		}
		return true, nil
	case tea.MouseActionRelease:
		if !m.selActive {
			return false, nil
		}
		m.selActive = false
		text := m.selectionText()
		if strings.TrimSpace(text) == "" {
			m.clearSelection() // bare click: no mark to keep
			return true, nil
		}
		// The mark stays highlighted until the next click or refresh; the
		// text is already on the clipboard the moment it is marked.
		if err := copyToClipboard(text); err != nil {
			m.setFlash("copy failed: "+err.Error(), true)
		} else {
			m.setFlash(fmt.Sprintf("copied (%s)", humanTokens(len(text))), false)
		}
		return true, nil
	}
	return false, nil
}

func (m *App) selBounds() (selPoint, selPoint) {
	a, b := m.selStart, m.selEnd
	if !a.before(b) {
		a, b = b, a
	}
	return a, b
}

// selectionText extracts the marked region from the plain transcript lines.
func (m *App) selectionText() string {
	a, b := m.selBounds()
	if a.line == b.line && a.col == b.col {
		return ""
	}
	var parts []string
	for l := a.line; l <= b.line && l < len(m.chatPlain); l++ {
		runes := []rune(m.chatPlain[l])
		start, end := 0, len(runes)
		if l == a.line {
			start = min(a.col, len(runes))
		}
		if l == b.line {
			end = min(b.col, len(runes))
		}
		if start > end {
			start = end
		}
		parts = append(parts, strings.TrimRight(string(runes[start:end]), " "))
	}
	return strings.Join(parts, "\n")
}

// applySelection re-renders the viewport with the marked region reversed.
// Marked lines lose their coloring while marked; everything else keeps its
// style.
func (m *App) applySelection() {
	a, b := m.selBounds()
	lines := make([]string, len(m.chatStyled))
	copy(lines, m.chatStyled)
	for l := a.line; l <= b.line && l < len(m.chatPlain); l++ {
		runes := []rune(m.chatPlain[l])
		start, end := 0, len(runes)
		if l == a.line {
			start = min(a.col, len(runes))
		}
		if l == b.line {
			end = min(b.col, len(runes))
		}
		if start > end {
			start = end
		}
		lines[l] = string(runes[:start]) +
			stySelected.Render(string(runes[start:end])) +
			string(runes[end:])
	}
	off := m.vp.YOffset
	m.vp.SetContent(strings.Join(lines, "\n"))
	m.vp.SetYOffset(off)
}

// clearSelection restores the styled transcript.
func (m *App) clearSelection() {
	off := m.vp.YOffset
	m.vp.SetContent(strings.Join(m.chatStyled, "\n"))
	m.vp.SetYOffset(off)
}
