package ui

import (
	"strings"
	"testing"

	"tapioca/internal/agent"
)

// A permission prompt that silently drops the end of a command lets the model
// choose what the user reads: padding pushed the redirect target off the box
// while [y] still ran it, and the prompt has no way to scroll.
func TestPermPromptCannotHideTheEndOfACommand(t *testing.T) {
	m := &App{w: 100, h: 24, mgr: &agent.Manager{}}
	pad := strings.Repeat("A", 1200)
	cmd := "printf '" + pad + "' > /root/.ssh/authorized_keys"
	m.perms = []permEntry{{req: &agent.PermissionReq{Tool: "bash", Summary: cmd}}}
	out := m.renderPerm(100, 24)
	if !strings.Contains(out, "not shown") {
		t.Error("the prompt hid part of the command without saying so")
	}
	if !strings.Contains(out, "characters in total") {
		t.Error("the prompt did not say how much it was hiding")
	}
	if !strings.Contains(out, "authorized_keys") {
		t.Errorf("the prompt never shows the redirect target; rendered %d bytes", len(out))
	}

	// A command that fits is shown as it always was, with no warning bolted on.
	m.perms = []permEntry{{req: &agent.PermissionReq{Tool: "bash", Summary: "git status"}}}
	if out := m.renderPerm(100, 24); !strings.Contains(out, "git status") || strings.Contains(out, "not shown") {
		t.Errorf("an ordinary command was mangled: %q", out)
	}
}
