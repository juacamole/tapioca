package ui

import (
	"testing"

	"tapioca/internal/config"
)

func layoutApp(panels []string, pos string) *App {
	return &App{
		w: 120, h: 40,
		cfg: &config.Config{Dashboard: config.DashboardConfig{
			Visible: true, Panels: panels, Width: 0.33, Position: pos,
		}},
	}
}

// An empty dashboard must not reserve space: the chat gets the whole screen.
func TestDashboardTakesNoSpaceWithoutPanels(t *testing.T) {
	for _, pos := range []string{"right", "left", "top", "bottom"} {
		if w, h := layoutApp(nil, pos).dashDims(); w != 0 || h != 0 {
			t.Errorf("%s: no panels still reserved %dx%d", pos, w, h)
		}
		// Panels that no longer exist resolve to nothing, same as none.
		if w, h := layoutApp([]string{"thinking", "gone"}, pos).dashDims(); w != 0 || h != 0 {
			t.Errorf("%s: unknown panels reserved %dx%d", pos, w, h)
		}
	}
}

func TestDashboardKeepsSpaceWithPanels(t *testing.T) {
	if w, _ := layoutApp([]string{"tokens"}, "right").dashDims(); w <= 0 {
		t.Error("a configured panel got no width")
	}
	if _, h := layoutApp([]string{"tokens"}, "bottom").dashDims(); h <= 0 {
		t.Error("a configured panel got no height")
	}
	// One unknown name among valid ones must not collapse the dashboard.
	if w, _ := layoutApp([]string{"gone", "tokens"}, "right").dashDims(); w <= 0 {
		t.Error("a valid panel was dropped because of an unknown sibling")
	}
}

func TestHiddenDashboardStillTakesNoSpace(t *testing.T) {
	m := layoutApp([]string{"tokens"}, "right")
	m.cfg.Dashboard.Visible = false
	if w, h := m.dashDims(); w != 0 || h != 0 {
		t.Errorf("hidden dashboard reserved %dx%d", w, h)
	}
}
