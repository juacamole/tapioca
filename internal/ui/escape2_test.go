package ui

import (
	"strings"
	"testing"
	"time"

	"tapioca/internal/agent"
	"tapioca/internal/config"
	"tapioca/internal/mcp"
	"tapioca/internal/stats"
	"tapioca/internal/tools"
)

// Sanitizing at a few choke points only works if every render site is one of
// them. These five were not: each draws a string the model or the tree chose,
// straight into the frame.

// The dashboard's tools panel draws the tool call's name. The transcript
// sanitizes the same name, and the arguments one line below it in this very
// function are sanitized — the name was simply missed.
func TestToolsPanelNameIsSanitized(t *testing.T) {
	m := &App{cfg: config.Default(), mgr: &agent.Manager{MCP: mcp.NewRegistry()}}
	a := &agent.Agent{}
	a.Stats.ToolCalls = []stats.ToolCallStat{{Name: nameIn + nasty, Args: "{}"}}
	out := strings.Join(renderToolsPanel(m, a, 40, 10), "\n")
	if hasEscape(out) {
		t.Fatalf("tools panel leaked an escape: %q", out)
	}
	if !strings.Contains(out, nameIn) {
		t.Fatalf("the readable part of the name was lost: %q", out)
	}
}

// The title bar draws the working directory every frame. A directory name may
// hold any byte but '/' and NUL, and an extracted tarball chooses it.
func TestTitleCwdIsSanitized(t *testing.T) {
	m := &App{
		cfg: config.Default(), w: 100,
		mgr: &agent.Manager{Exec: tools.NewExecutor("/tmp/work"+nasty, "manual")},
	}
	if out := m.renderTitle(); hasEscape(out) {
		t.Fatalf("title bar leaked an escape: %q", out)
	}
}

// The @-mention menu draws candidate file names, which in a hostile tree are
// whatever the archive was built with.
func TestMentionMenuIsSanitized(t *testing.T) {
	m := &App{cfg: config.Default(), w: 80, h: 24}
	if out := m.renderMentionMenu([]string{"notes" + nasty + ".md"}, 60); hasEscape(out) {
		t.Fatalf("mention menu leaked an escape: %q", out)
	}
}

// openTextOverlay sanitizes the body but not the heading, and /diff and
// /remember both build the heading out of the working directory.
func TestTextOverlayTitleIsSanitized(t *testing.T) {
	m := &App{cfg: config.Default(), w: 80, h: 24}
	m.openTextOverlay("git diff — /tmp/work"+nasty, "body")
	if out := m.renderTextOverlay(80, 24); hasEscape(out) {
		t.Fatalf("overlay title leaked an escape: %q", out)
	}
}

// The retry note is built from a provider error and then drawn in the header,
// the agents panel and the status bar.
func TestRetryNoteIsSanitized(t *testing.T) {
	a := &agent.Agent{
		Status:       agent.StatusWaiting,
		RetryAt:      time.Now().Add(5 * time.Second),
		RetryNote:    "1/3 " + nasty,
		StatusDetail: "running bash " + nasty,
	}
	if out := statusLabel(a); hasEscape(out) {
		t.Fatalf("status label leaked an escape: %q", out)
	}
}
