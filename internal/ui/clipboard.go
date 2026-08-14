package ui

import (
	"encoding/base64"
	"fmt"
	"os"
	"os/exec"
	"strings"

	tea "charm.land/bubbletea/v2"

	"tapioca/internal/agent"
)

// copyToClipboard tries native clipboard tools (wayland/X11/mac), then falls
// back to an OSC 52 escape, which most terminals translate into a clipboard
// write even over ssh.
func copyToClipboard(text string) error {
	candidates := [][]string{
		{"wl-copy"},
		{"xclip", "-selection", "clipboard"},
		{"xsel", "-ib"},
		{"pbcopy"},
	}
	for _, c := range candidates {
		if _, err := exec.LookPath(c[0]); err != nil {
			continue
		}
		cmd := exec.Command(c[0], c[1:]...)
		cmd.Stdin = strings.NewReader(text)
		if cmd.Run() == nil {
			return nil
		}
	}
	b64 := base64.StdEncoding.EncodeToString([]byte(text))
	_, err := os.Stdout.WriteString("\x1b]52;c;" + b64 + "\x07")
	return err
}

// copyLast copies the newest assistant response of the active agent.
func (m *App) copyLast() tea.Cmd {
	a := m.mgr.ActiveAgent()
	if a == nil {
		return nil
	}
	text := a.StreamText
	if text == "" {
		for i := len(a.Messages) - 1; i >= 0; i-- {
			if a.Messages[i].Role == "assistant" {
				if t := a.Messages[i].Text(); strings.TrimSpace(t) != "" {
					text = t
					break
				}
			}
		}
	}
	if strings.TrimSpace(text) == "" {
		m.setFlash("nothing to copy yet", true)
		return m.flashCmd()
	}
	if err := copyToClipboard(text); err != nil {
		m.setFlash("copy failed: "+err.Error(), true)
	} else {
		m.setFlash(fmt.Sprintf("copied last response (%s chars)", humanInt(len(text))), false)
	}
	return m.flashCmd()
}

// copyAll copies the whole transcript of the active agent as plain text.
func (m *App) copyAll() tea.Cmd {
	a := m.mgr.ActiveAgent()
	if a == nil {
		return nil
	}
	text := transcriptText(a)
	if strings.TrimSpace(text) == "" {
		m.setFlash("nothing to copy yet", true)
		return m.flashCmd()
	}
	if err := copyToClipboard(text); err != nil {
		m.setFlash("copy failed: "+err.Error(), true)
	} else {
		m.setFlash(fmt.Sprintf("copied transcript (%s chars)", humanInt(len(text))), false)
	}
	return m.flashCmd()
}

// transcriptText renders the conversation as unstyled text.
func transcriptText(a *agent.Agent) string {
	var b strings.Builder
	for _, msg := range a.Messages {
		if msg.IsToolResult() {
			for _, bl := range msg.Blocks {
				fmt.Fprintf(&b, "[%s result]\n%s\n\n", bl.Name, bl.Content)
			}
			continue
		}
		who := "you"
		if msg.Role == "assistant" {
			who = a.Name
		}
		fmt.Fprintf(&b, "## %s\n", who)
		for _, bl := range msg.Blocks {
			switch bl.Type {
			case "thinking":
				fmt.Fprintf(&b, "[thinking]\n%s\n", bl.Text)
			case "text":
				b.WriteString(bl.Text + "\n")
			case "tool_use":
				fmt.Fprintf(&b, "[tool %s] %s\n", bl.Name, string(bl.Input))
			}
		}
		b.WriteString("\n")
	}
	return b.String()
}
