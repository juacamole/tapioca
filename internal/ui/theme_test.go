package ui

import (
	"strings"
	"testing"

	"charm.land/lipgloss/v2"
)

func restoreLook(t *testing.T) {
	t.Helper()
	t.Cleanup(func() {
		SetTheme(defaultTheme, nil)
		SetGlyphs(defaultGlyphs)
	})
}

func TestSetThemeFallsBackOnUnknownName(t *testing.T) {
	restoreLook(t)
	if got := SetTheme("does-not-exist", nil); got != defaultTheme {
		t.Errorf("unknown theme resolved to %q, want %q", got, defaultTheme)
	}
	if got := SetTheme("CONTRAST", nil); got != "contrast" {
		t.Errorf("theme names should be case-insensitive, got %q", got)
	}
}

func TestColorOverrides(t *testing.T) {
	restoreLook(t)
	SetTheme("taro", map[string]string{
		"accent": "#111111/#222222",
		"error":  "#ABCDEF",
		"agents": "#010101, #020202",
		"bogus":  "#FFFFFF",
	})
	if active.accent.Light != "#111111" || active.accent.Dark != "#222222" {
		t.Errorf("light/dark pair not parsed: %+v", active.accent)
	}
	if active.err.Light != "#ABCDEF" || active.err.Dark != "#ABCDEF" {
		t.Errorf("single hex should apply to both backgrounds: %+v", active.err)
	}
	if len(active.agents) != 2 || active.agents[1].Dark != "#020202" {
		t.Errorf("agent color list not parsed: %+v", active.agents)
	}
}

func TestMonoThemeEmitsNoColor(t *testing.T) {
	restoreLook(t)
	SetTheme("mono", nil)
	if !active.mono {
		t.Fatal("mono theme did not take effect")
	}
	if _, isNoColor := agentColor(1).(lipgloss.NoColor); !isNoColor {
		t.Error("mono theme still assigns agent colors")
	}
	// A named color means the user wants color after all.
	SetTheme("mono", map[string]string{"accent": "#123456"})
	if active.mono {
		t.Error("an explicit color override should lift mono")
	}
}

func TestGlyphSetsCoverEveryField(t *testing.T) {
	restoreLook(t)
	for _, name := range glyphNames() {
		g := glyphSets[name]
		for field, val := range map[string]string{
			"sep": g.sep, "sepTight": g.sepTight, "ellipsis": g.ellipsis,
			"dot": g.dot, "check": g.check, "caret": g.caret, "bar": g.bar,
			"line": g.line, "gaugeFull": g.gaugeFull, "gaugeEmpty": g.gaugeEmpty,
			"toolOK": g.toolOK, "toolErr": g.toolErr,
			"todoDone": g.todoDone, "todoDoing": g.todoDoing, "todoWait": g.todoWait,
		} {
			if val == "" {
				t.Errorf("glyph set %q has an empty %s", name, field)
			}
		}
		if len(g.spark) != 8 {
			t.Errorf("glyph set %q has %d sparkline levels, want 8", name, len(g.spark))
		}
		if len(g.spinner.Frames) == 0 {
			t.Errorf("glyph set %q has no spinner frames", name)
		}
	}
}

func TestAsciiGlyphsAreActuallyAscii(t *testing.T) {
	restoreLook(t)
	SetGlyphs("ascii")
	parts := []string{gl.sep, gl.sepTight, gl.ellipsis, gl.dot, gl.check, gl.caret,
		gl.bar, gl.line, gl.gaugeFull, gl.gaugeEmpty, gl.toolOK, gl.toolErr,
		gl.todoDone, gl.todoDoing, gl.todoWait, string(gl.spark)}
	parts = append(parts, gl.spinner.Frames...)
	for _, b := range []lipgloss.Border{gl.border, gl.focusBorder} {
		parts = append(parts, b.Top, b.Bottom, b.Left, b.Right,
			b.TopLeft, b.TopRight, b.BottomLeft, b.BottomRight)
	}
	for _, s := range parts {
		for _, r := range s {
			if r > 127 {
				t.Errorf("ascii glyph set contains non-ASCII %q in %q", r, s)
			}
		}
	}
}

// truncate reserves room for the ellipsis, which is 3 characters in ascii.
func TestTruncateRespectsGlyphWidth(t *testing.T) {
	restoreLook(t)
	for _, name := range []string{"unicode", "ascii"} {
		SetGlyphs(name)
		for _, n := range []int{1, 2, 3, 4, 10} {
			got := truncate(strings.Repeat("x", 40), n)
			if w := lipgloss.Width(got); w > n {
				t.Errorf("%s: truncate(.., %d) produced width %d (%q)", name, n, w, got)
			}
		}
	}
}
