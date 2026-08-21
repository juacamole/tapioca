package ui

import (
	"strings"
	"testing"

	"charm.land/lipgloss/v2"

	"tapioca/internal/agent"
)

// The sparkline in the stats panel is drawn from the out-token count of every
// request in the session. Those counts come from the model server, which is one
// of the two untrusted inputs, and the scale is
//
//	idx := v * (len(chars) - 1) / maxV
//
// with maxV floored at 1 and never at v. A negative count therefore produces a
// negative index into a rune slice of eight, and the panic takes the whole
// program down — not a render glitch, the process.
//
// The counts are clamped where they enter now, which is the fix. This is the
// other end of it: a session saved before that was true loads its Stats
// straight off disk, and drawing a chart must not be the thing that kills the
// app.
func TestTheSparklineSurvivesACountItDidNotChoose(t *testing.T) {
	for _, tc := range []struct {
		name string
		vals []int
	}{
		{"a negative count among real ones", []int{120, -5, 340}},
		{"every count negative", []int{-1, -2, -3}},
		{"a huge count", []int{1, 1 << 62}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := sparkline(tc.vals, 40)
			if n := len([]rune(got)); n != len(tc.vals) {
				t.Fatalf("sparkline drew %d cells for %d values: %q", n, len(tc.vals), got)
			}
		})
	}
}

// The ordinary half: the chart still has to be a chart. A rising series must
// rise, and the tallest value must reach the top of the glyph range.
func TestTheSparklineStillDrawsAnOrdinarySeries(t *testing.T) {
	got := []rune(sparkline([]int{0, 100, 200, 400}, 40))
	if len(got) != 4 {
		t.Fatalf("sparkline drew %q", string(got))
	}
	for i := 1; i < len(got); i++ {
		if strings.IndexRune(string(gl.spark), got[i]) < strings.IndexRune(string(gl.spark), got[i-1]) {
			t.Fatalf("a rising series did not rise: %q", string(got))
		}
	}
	if got[len(got)-1] != []rune(gl.spark)[len([]rune(gl.spark))-1] {
		t.Errorf("the largest value did not reach the top of the range: %q", string(got))
	}
}

// The permission box is the one place the user decides anything, and it cannot
// scroll. Its summary was given a careful truncation because a long command let
// the model choose which part of it was on screen. The line above the summary —
// "tool: …" — is model-chosen too and had no bound at all: an MCP call's key is
// "mcp:" plus whatever name the model emitted, and agent dispatch sends an
// unrecognised name down that same branch, so no server has to be involved.
//
// A name of a few thousand characters made every line of the box that wide.
// In a real terminal each line then wraps many times over, so a box that is
// twenty-four rows tall becomes hundreds, and the summary and the [y]/[a]/[n]
// footer are pushed off the screen — while [y] still runs the call.
func TestThePermissionBoxFitsTheScreenWhateverTheToolIsCalled(t *testing.T) {
	const w, h = 100, 24
	m := &App{w: w, h: h, mgr: &agent.Manager{}}
	name := "mcp:server__" + strings.Repeat("z", 3000)
	m.perms = []permEntry{{req: &agent.PermissionReq{Tool: name, Summary: "rm -rf /home/you"}}}

	out := m.renderPerm(w, h)
	if got := lipgloss.Width(out); got > w {
		t.Errorf("the box is %d columns wide on a %d column screen", got, w)
	}
	for i, line := range strings.Split(out, "\n") {
		if got := lipgloss.Width(line); got > w {
			t.Fatalf("line %d is %d columns wide on a %d column screen", i, got, w)
		}
	}
	// Truncated, but still identified and still decidable.
	if !strings.Contains(out, "mcp:server__") {
		t.Error("the box no longer says which tool is being asked about")
	}
	if !strings.Contains(out, "rm -rf /home/you") {
		t.Error("the summary was pushed out of the box by the tool name")
	}
	for _, key := range []string{"[y]", "[a]", "[n]"} {
		if !strings.Contains(out, key) {
			t.Errorf("the %s choice is not in the box", key)
		}
	}
}

// The ordinary half: a normal prompt is unchanged, name and all.
func TestAnOrdinaryPermissionBoxIsUnchanged(t *testing.T) {
	m := &App{w: 100, h: 24, mgr: &agent.Manager{}}
	m.perms = []permEntry{{req: &agent.PermissionReq{Tool: "bash", Summary: "git status"}}}
	out := m.renderPerm(100, 24)
	if !strings.Contains(out, "tool: bash") || !strings.Contains(out, "git status") {
		t.Errorf("an ordinary prompt was mangled: %q", out)
	}
	if strings.Contains(out, gl.ellipsis+" ") {
		t.Error("an ordinary prompt was truncated")
	}
}

// The box has to hold together at any terminal size, not just the one it is
// usually looked at. A width clamp that is wider than the box it sits in would
// clip the end of a command instead of wrapping it, which is the failure
// TestPermPromptCannotHideTheEndOfACommand exists to prevent.
func TestThePermissionBoxFitsEveryScreenWidth(t *testing.T) {
	for _, w := range []int{20, 26, 40, 60, 80, 100, 200} {
		m := &App{w: w, h: 24, mgr: &agent.Manager{}}
		m.perms = []permEntry{{req: &agent.PermissionReq{
			Tool: "bash", Summary: "printf '" + strings.Repeat("A", 300) + "' > /root/.ssh/authorized_keys"}}}
		out := m.renderPerm(w, 24)
		for i, line := range strings.Split(out, "\n") {
			if got := lipgloss.Width(line); got > w {
				t.Errorf("w=%d: line %d is %d columns wide", w, i, got)
			}
		}
		// Wide enough to read on, the redirect target still has to be visible.
		if w >= 60 && !strings.Contains(out, "authorized_keys") {
			t.Errorf("w=%d: the redirect target is not on screen", w)
		}
	}
}
