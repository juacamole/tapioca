package ui

import (
	"context"
	"strings"

	tea "charm.land/bubbletea/v2"

	"tapioca/internal/config"
	"tapioca/internal/mcp"
)

// A server behind OAuth cannot be connected by editing the config: the token
// does not exist until someone has consented to it in a browser. /mcp is that
// consent. It is a command rather than something startup does on its own,
// because a browser window opening by itself while the app starts is not a
// login anybody asked for — and on a machine with no browser it is a wait for
// something that will never happen.

// cmdMCPLogin runs the browser login for one server and reconnects it.
func cmdMCPLogin(m *App, arg string) tea.Cmd {
	var servers []config.MCPServerConfig
	for _, sc := range m.cfg.MCP {
		if mcp.UsesOAuth(sc) {
			servers = append(servers, sc)
		}
	}
	if len(servers) == 0 {
		m.setFlash(`no [[mcp]] entry has auth = "oauth"`, true)
		return m.flashCmd()
	}
	target, ok := pickMCPServer(servers, arg)
	if !ok {
		names := make([]string, 0, len(servers))
		for _, sc := range servers {
			names = append(names, sanitizeLabel(sc.Name))
		}
		m.setFlash("which server? /mcp "+strings.Join(names, " | "), true)
		return m.flashCmd()
	}
	m.setFlash("mcp "+sanitizeLabel(target.Name)+": asking the server who authorizes it…", false)
	return tea.Batch(startMCPLoginCmd(target), m.flashCmd())
}

// pickMCPServer resolves the argument. With one server configured the argument
// is optional; with several it is not, since logging in to the wrong one costs
// a consent screen and leaves the intended server as broken as it was.
func pickMCPServer(servers []config.MCPServerConfig, arg string) (config.MCPServerConfig, bool) {
	arg = strings.TrimSpace(arg)
	if arg == "" {
		if len(servers) == 1 {
			return servers[0], true
		}
		return config.MCPServerConfig{}, false
	}
	for _, sc := range servers {
		if strings.EqualFold(sc.Name, arg) || mcp.Sanitize(sc.Name) == arg {
			return sc, true
		}
	}
	return config.MCPServerConfig{}, false
}

// mcpAuthMsg carries a started login back to the update loop. Discovery and
// registration are several requests to a host that may be slow or down, so they
// happen off the loop like every other network call.
type mcpAuthMsg struct {
	server config.MCPServerConfig
	auth   *mcp.Authorization
	err    error
}

// mcpAuthDoneMsg is the outcome of the consent.
type mcpAuthDoneMsg struct {
	server config.MCPServerConfig
	err    error
}

func startMCPLoginCmd(sc config.MCPServerConfig) tea.Cmd {
	return func() tea.Msg {
		auth, err := mcp.StartLogin(context.Background(), sc)
		return mcpAuthMsg{server: sc, auth: auth, err: err}
	}
}

// loginOverlayTitle names the overlay holding a login URL, so the login that
// opened it is the only thing that closes it.
func loginOverlayTitle(name string) string { return "mcp " + sanitizeLabel(name) + " login" }

func (m *App) handleMCPAuth(msg mcpAuthMsg) tea.Cmd {
	if msg.err != nil {
		m.setFlash("mcp "+sanitizeLabel(msg.server.Name)+": "+sanitizeLabel(msg.err.Error()), true)
		return m.flashCmd()
	}
	if err := mcp.OpenBrowser(msg.auth.URL); err != nil {
		// No browser to open here. The address is the entire login, so it goes
		// somewhere it can be read and copied rather than into a flash that
		// expires while the user is finding another machine.
		m.openTextOverlay(loginOverlayTitle(msg.server.Name),
			"Open this address to authorize "+msg.server.Name+":\n\n"+msg.auth.URL)
	} else {
		m.setFlash("mcp "+sanitizeLabel(msg.server.Name)+": waiting for consent in your browser…", false)
	}
	return tea.Batch(waitMCPLoginCmd(msg.server, msg.auth), m.flashCmd())
}

func waitMCPLoginCmd(sc config.MCPServerConfig, auth *mcp.Authorization) tea.Cmd {
	return func() tea.Msg {
		return mcpAuthDoneMsg{server: sc, err: auth.Wait(context.Background())}
	}
}

func (m *App) handleMCPAuthDone(msg mcpAuthDoneMsg) tea.Cmd {
	if m.overlay == overlayText && m.textTitle == loginOverlayTitle(msg.server.Name) {
		m.dismissOverlay()
	}
	if msg.err != nil {
		m.setFlash(sanitizeLabel(msg.err.Error()), true)
		return m.flashCmd()
	}
	m.setFlash("mcp "+sanitizeLabel(msg.server.Name)+" authorized — connecting…", false)
	return tea.Batch(reconnectMCPCmd(m.mgr.MCP, msg.server), m.flashCmd())
}

// reconnectMCPCmd connects a server that is already in the registry as a
// failure. It replaces rather than adds: the entry that failed at startup is
// still listed, and a second client under the same name offers every tool
// twice.
func reconnectMCPCmd(reg *mcp.Registry, sc config.MCPServerConfig) tea.Cmd {
	return func() tea.Msg {
		c, err := mcp.Start(context.Background(), sc)
		if err != nil {
			reg.SetError(sc.Name, err.Error())
			return mcpConnectedMsg{name: sc.Name, err: err}
		}
		reg.Replace(c)
		return mcpConnectedMsg{name: sc.Name, tools: len(c.Tools())}
	}
}
