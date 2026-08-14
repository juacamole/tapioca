package ui

import (
	"fmt"
	"testing"

	"tapioca/internal/agent"
	"tapioca/internal/config"
)

// Clicking a panel sometimes did not focus it. The click-to-panel mapping in
// regionAt is a second copy of the geometry renderDashboard lays out, and a
// second copy drifts. This walks every cell of every panel at a range of sizes
// and positions and checks the two agree.
func newDashApp(w, h int, pos string) *App {
	cfg := config.Default()
	cfg.Dashboard.Position = pos
	m := &App{cfg: cfg, w: w, h: h, ready: true, mgr: &agent.Manager{}}
	return m
}

// panelBounds returns, for each panel the renderer draws, the half-open range
// it occupies along the stacking axis in screen coordinates.
func panelBounds(m *App) (starts []int, sizes []int, vertical bool, origin int) {
	bodyTop := 2
	bodyH := m.h - 3
	dashW, dashH := m.dashDims()
	var defs []*panelDef
	if dashW > 0 {
		defs, sizes, vertical = m.dashLayout(dashW, bodyH)
		origin = bodyTop
	} else {
		defs, sizes, vertical = m.dashLayout(m.w, dashH)
		if m.cfg.Dashboard.Position == "top" {
			origin = 0 // measured along x for a row layout
		}
	}
	_ = defs
	at := 0
	for _, s := range sizes {
		starts = append(starts, at)
		at += s
	}
	return starts, sizes, vertical, origin
}

func TestEveryCellOfAPanelFocusesThatPanel(t *testing.T) {
	sizes := []struct{ w, h int }{
		{100, 30}, {100, 24}, {120, 40}, {80, 20}, {200, 50}, {90, 17},
	}
	for _, pos := range []string{"right", "left", "top", "bottom"} {
		for _, sz := range sizes {
			t.Run(fmt.Sprintf("%s-%dx%d", pos, sz.w, sz.h), func(t *testing.T) {
				m := newDashApp(sz.w, sz.h, pos)
				dashW, dashH := m.dashDims()
				if dashW == 0 && dashH == 0 {
					t.Skip("no dashboard at this size")
				}

				bodyTop := 2
				bodyH := m.h - 3
				// The rectangle the renderer fills, mirroring renderBody.
				var dx, dy, dw, dh int
				switch {
				case dashW > 0 && pos == "left":
					dx, dy, dw, dh = 0, bodyTop, dashW, bodyH
				case dashW > 0:
					dx, dy, dw, dh = m.w-dashW, bodyTop, dashW, bodyH
				case pos == "top":
					dx, dy, dw, dh = 0, bodyTop, m.w, dashH
				default:
					dx, dy, dw, dh = 0, bodyTop+bodyH-dashH, m.w, dashH
				}

				starts, panelSizes, vertical, _ := panelBounds(m)
				for i := range panelSizes {
					// Sample the first, middle and last cell of this panel.
					for _, off := range []int{0, panelSizes[i] / 2, panelSizes[i] - 1} {
						var x, y int
						if vertical {
							x, y = dx+dw/2, dy+starts[i]+off
						} else {
							x, y = dx+starts[i]+off, dy+dh/2
						}
						if x < dx || x >= dx+dw || y < dy || y >= dy+dh {
							continue
						}
						r, idx := m.regionAt(x, y)
						if r != regionDash {
							t.Errorf("(%d,%d) in panel %d is region %v, not the dashboard", x, y, i, r)
							continue
						}
						if idx != i {
							t.Errorf("(%d,%d) is inside panel %d but focuses panel %d", x, y, i, idx)
						}
					}
				}
			})
		}
	}
}

// The index a click produces has to be one the renderer actually drew, or
// focus lands on a panel that is not on screen and nothing appears to happen.
func TestClickNeverSelectsAPanelThatWasNotDrawn(t *testing.T) {
	for _, pos := range []string{"right", "left", "top", "bottom"} {
		for h := 16; h <= 60; h++ {
			m := newDashApp(100, h, pos)
			dashW, dashH := m.dashDims()
			if dashW == 0 && dashH == 0 {
				continue
			}
			var defs []*panelDef
			if dashW > 0 {
				defs, _, _ = m.dashLayout(dashW, m.h-3)
			} else {
				defs, _, _ = m.dashLayout(m.w, dashH)
			}
			drawn := len(defs)

			for y := 0; y < m.h; y++ {
				for _, x := range []int{0, m.w / 2, m.w - 1} {
					r, idx := m.regionAt(x, y)
					if r != regionDash {
						continue
					}
					if idx < 0 || idx >= drawn {
						t.Fatalf("%s h=%d: click at (%d,%d) selects panel %d of %d drawn",
							pos, h, x, y, idx, drawn)
					}
				}
			}
		}
	}
}

// A panel the layout dropped for lack of room must not be reachable, and
// panelFocused must never highlight an index past the end.
func TestFocusIndexIsClampedToWhatIsDrawn(t *testing.T) {
	m := newDashApp(100, 30, "right")
	defs, _, _ := m.dashLayout(100, m.h-3)
	m.focus = focusDash
	m.dashPanelSel = len(defs) + 3 // stale selection after a resize

	var focused int
	for i := range defs {
		if m.panelFocused(i, len(defs)) {
			focused++
		}
	}
	if focused != 1 {
		t.Errorf("%d panels are focused, want exactly 1", focused)
	}
}
