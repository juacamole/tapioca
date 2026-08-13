package ui

import (
	"github.com/charmbracelet/bubbles/textarea"
	"github.com/charmbracelet/bubbles/viewport"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"tapioca/internal/agent"
	"tapioca/internal/config"
	"tapioca/internal/mcp"
)

// press builds the message bubbletea delivers for a left click.
func press(x, y int) tea.MouseMsg {
	return tea.MouseMsg{X: x, Y: y, Action: tea.MouseActionPress, Button: tea.MouseButtonLeft}
}

func dashApp(t *testing.T, w, h int) *App {
	t.Helper()
	cfg := config.Default()
	mgr := agent.NewManager(cfg, mcp.NewRegistry(), nil)
	mgr.Agents = []*agent.Agent{{ID: 1}}
	m := &App{cfg: cfg, w: w, h: h, ready: true, mgr: mgr}
	m.keys = NewKeyMap(nil)
	m.ta = textarea.New()
	m.vp = viewport.New(w, h)
	m.recalcLayout()
	return m
}

// Clicking a panel must focus it — every panel, not most of them.
func TestClickingEachPanelFocusesIt(t *testing.T) {
	m := dashApp(t, 100, 46)
	dashW, _ := m.dashDims()
	if dashW == 0 {
		t.Fatal("no dashboard")
	}
	defs, sizes, _ := m.dashLayout(dashW, m.h-3)
	x := m.w - dashW/2

	at := 2 // bodyTop
	for i := range defs {
		y := at + sizes[i]/2
		at += sizes[i]

		m.focus = focusInput
		m.Update(press(x, y))

		if m.focus != focusDash {
			t.Errorf("clicking panel %d (%s) at y=%d left focus on %v", i, defs[i].key, y, m.focus)
		}
		if m.dashPanelSel != i {
			t.Errorf("clicking panel %d (%s) selected panel %d", i, defs[i].key, m.dashPanelSel)
		}
	}
}

// Moving from one panel to another is the case in the report: focus is already
// on the dashboard and the click has to move the selection.
func TestClickingAnotherPanelMovesTheSelection(t *testing.T) {
	m := dashApp(t, 100, 46)
	dashW, _ := m.dashDims()
	defs, sizes, _ := m.dashLayout(dashW, m.h-3)
	x := m.w - dashW/2

	starts := make([]int, len(sizes))
	at := 2
	for i, s := range sizes {
		starts[i] = at
		at += s
	}

	for from := range defs {
		for to := range defs {
			m.focus = focusDash
			m.dashPanelSel = from
			m.Update(press(x, starts[to]+sizes[to]/2))
			if m.dashPanelSel != to {
				t.Errorf("from panel %d, clicking panel %d selected %d", from, to, m.dashPanelSel)
			}
			if m.focus != focusDash {
				t.Errorf("from panel %d, clicking panel %d lost dashboard focus", from, to)
			}
		}
	}
}

// A click while the settings panel is being edited must still move focus; it
// used to be possible to get stuck editing one panel while clicking another.
func TestClickingAPanelWhileEditingStillMoves(t *testing.T) {
	m := dashApp(t, 100, 46)
	dashW, _ := m.dashDims()
	defs, sizes, _ := m.dashLayout(dashW, m.h-3)
	x := m.w - dashW/2

	m.focus = focusDash
	m.dashPanelSel = len(defs) - 1
	m.dashEditing = true

	m.Update(press(x, 2+sizes[0]/2))
	if m.dashPanelSel != 0 {
		t.Errorf("selection stayed on %d while editing", m.dashPanelSel)
	}
	if m.dashEditing {
		t.Error("still editing after clicking a different panel")
	}
}

// Every row of the dashboard belongs to some panel. A row that maps to no
// panel is a click that appears to do nothing.
func TestNoDeadRowsInsideTheDashboard(t *testing.T) {
	for _, h := range []int{20, 24, 30, 40, 46, 60} {
		m := dashApp(t, 100, h)
		dashW, _ := m.dashDims()
		if dashW == 0 {
			continue
		}
		x := m.w - dashW/2
		bodyTop, bodyH := 2, m.h-3
		for y := bodyTop; y < bodyTop+bodyH; y++ {
			m.focus = focusInput
			m.Update(press(x, y))
			if m.focus != focusDash {
				t.Errorf("h=%d: clicking dashboard row y=%d did not focus a panel", h, y)
			}
		}
	}
}

// "click any pane to focus it" is what the help promises. The chat pane's own
// border rows and the rule between the transcript and the input mapped to no
// region at all, so a click that lands on one silently does nothing — which is
// exactly the "sometimes it doesn't work" in the report, since whether you hit
// a border is a matter of a row or two.
func TestNoDeadRowsInsideTheChatPane(t *testing.T) {
	for _, h := range []int{24, 30, 46} {
		m := dashApp(t, 100, h)
		dashW, _ := m.dashDims()
		x := (m.w - dashW) / 2 // middle of the chat pane

		bodyTop, bodyH := 2, m.h-3
		var dead []int
		for y := bodyTop; y < bodyTop+bodyH; y++ {
			if r, _ := m.regionAt(x, y); r == regionNone {
				dead = append(dead, y)
			}
		}
		if len(dead) > 0 {
			t.Errorf("h=%d: rows %v of the chat pane belong to no region; a click there does nothing", h, dead)
		}
	}
}
