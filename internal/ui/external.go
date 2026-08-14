package ui

import (
	"context"
	"os/exec"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

	"tapioca/internal/acp"
	"tapioca/internal/config"
)

// An external agent is another program that speaks the Agent Client Protocol.
// Connecting one gives it a tab: prompts typed there go to it, its work streams
// into the transcript, and everything it asks to run goes through this
// session's permission rules — the whole point of driving it from here rather
// than in another terminal.

// externalPick marks a picker value as an agent to connect rather than a
// provider to send to.
const externalPick = "agent\x1f"

// externalDialTimeout bounds the handshake. A command that is not an ACP agent
// usually says nothing at all, and waiting for it forever looks like a hang.
const externalDialTimeout = 30 * time.Second

type externalConnectedMsg struct {
	name   string
	client *acp.Client
	err    error
}

// externalAgent finds a configured entry by name.
func (m *App) externalAgent(name string) (config.ExternalAgent, bool) {
	for _, e := range m.cfg.Agents.External {
		if strings.EqualFold(e.Name, name) {
			return e, true
		}
	}
	return config.ExternalAgent{}, false
}

// externalEntries lists the configured agents for the connect screen. The
// probe is whether the command exists: launching one to find out would start a
// process for every entry every time the screen opens.
func externalEntries(cfg *config.Config) []connEntry {
	out := make([]connEntry, 0, len(cfg.Agents.External))
	for i := range cfg.Agents.External {
		e := cfg.Agents.External[i]
		entry := connEntry{name: e.Name, external: &cfg.Agents.External[i]}
		switch {
		case strings.TrimSpace(e.Command) == "":
			entry.state, entry.detail = connUnset, "no command configured"
		default:
			if _, err := exec.LookPath(e.Command); err != nil {
				entry.state, entry.detail = connFailing, e.Command+" is not on PATH"
			} else {
				entry.state, entry.detail = connReady, "ready"+gl.sep+"agent over ACP"
			}
		}
		out = append(out, entry)
	}
	return out
}

// connectExternal starts the connection. It runs off the UI goroutine: a
// process launch and a handshake are not instant, and the frontend must keep
// drawing the agents already running.
func (m *App) connectExternal(name string) tea.Cmd {
	ec, ok := m.externalAgent(name)
	if !ok {
		m.setFlash("no external agent named "+name+" — add one under [[agents.external]]", true)
		return m.flashCmd()
	}
	cwd := m.cwd()
	gate := m.mgr.Exec
	m.setFlash("connecting to "+ec.Name+"…", false)
	return tea.Batch(func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), externalDialTimeout)
		defer cancel()
		c, err := acp.Dial(ctx, ec, cwd, gate)
		return externalConnectedMsg{name: ec.Name, client: c, err: err}
	}, m.flashCmd())
}

// handleExternalConnected gives a connected agent its tab. The tab is created
// only once the handshake worked, so a failed connection leaves nothing behind
// to explain.
func (m *App) handleExternalConnected(msg externalConnectedMsg) (tea.Model, tea.Cmd) {
	if msg.err != nil {
		m.setFlash("connecting to "+msg.name+": "+msg.err.Error(), true)
		return m, m.flashCmd()
	}
	a := m.mgr.NewExternal(msg.name, msg.client)
	msg.client.Attach(a)
	m.mgr.Active = len(m.mgr.Agents) - 1
	m.dirty = true
	m.refreshChat(true)
	m.setFlash("connected to "+msg.name+gl.sep+"prompts in this tab go to it", false)
	return m, tea.Batch(waitAgent(a), m.flashCmd())
}
