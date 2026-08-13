package ui

import (
	"errors"
	"os/exec"

	tea "github.com/charmbracelet/bubbletea"

	"tapioca/internal/config"
	"tapioca/internal/provider"
)

// Anthropic can be connected two ways, and neither is obviously right for
// everyone: logging in avoids managing a static key but needs the Anthropic
// CLI installed, while a pasted key needs nothing but is a secret to look
// after. So the choice is offered rather than decided.

const (
	authMethodOAuth = "oauth"
	authMethodKey   = "key"
)

// openAuthMethodPicker asks how to connect. The login option reports up front
// whether it can actually be used — offering a path that will fail on
// selection is worse than not offering it, because the user has to discover
// the prerequisite by hitting it.
func (m *App) openAuthMethodPicker(k provider.Kind) {
	ok, why := provider.AnthropicOAuthAvailable()
	loginDesc := why
	if ok {
		loginDesc = "use your Anthropic account" + gl.sep + why
	}
	items := []pickerItem{
		{label: "log in", desc: loginDesc, value: authMethodOAuth},
		{label: "API key", desc: "paste a key from " + k.URL, value: authMethodKey},
	}
	m.credKind = k
	m.pick = newPicker(pickAuthMethod, "connect "+k.Label, items)
	m.overlay = overlayPicker
}

// loginDoneMsg carries the result of the browser login back to the update
// loop, after the TUI has been restored.
type loginDoneMsg struct {
	kind string
	err  error
}

// runLogin performs the browser login. It suspends the TUI and gives the CLI
// the terminal, the same way the editor is run: the CLI prints a URL and waits
// for the callback, and on a machine where no browser can be opened that URL
// is the only way through. A background process would swallow it.
//
// No timeout: the user has to get through a browser flow, which can take
// minutes, and a deadline that assumes otherwise would cancel a login in
// progress.
func runLogin(k provider.Kind) tea.Cmd {
	return tea.ExecProcess(provider.LoginCommand(), func(err error) tea.Msg {
		if err != nil {
			return loginDoneMsg{kind: k.Type, err: err}
		}
		// Exiting zero is not proof of a session: a cancelled flow can still
		// exit cleanly. Ask for a token, which is the thing that has to work.
		if ok, why := provider.AnthropicOAuthAvailable(); !ok {
			return loginDoneMsg{kind: k.Type, err: errors.New(why)}
		}
		return loginDoneMsg{kind: k.Type}
	})
}

// handleLoginDone records the outcome. A login that failed must not leave the
// provider configured to use one.
func (m *App) handleLoginDone(msg loginDoneMsg) tea.Cmd {
	k, ok := provider.KindFor(msg.kind)
	if !ok {
		return nil
	}
	if msg.err != nil {
		m.setFlash("login failed"+gl.sep+sanitizeLabel(msg.err.Error()), true)
		return m.flashCmd()
	}
	return m.saveOAuth(k)
}

// applyAuthMethod acts on the chosen method.
func (m *App) applyAuthMethod(method string) tea.Cmd {
	k := m.credKind
	if method == authMethodKey {
		m.openCredentialEntry(k)
		return nil
	}

	// Already logged in: nothing to do but record it. Otherwise run the login
	// rather than reporting that there is none — a button labelled "log in"
	// that only checks for a login is not a button that logs you in.
	if ok, _ := provider.AnthropicOAuthAvailable(); ok {
		return m.saveOAuth(k)
	}
	if _, err := exec.LookPath("ant"); err != nil {
		_, why := provider.AnthropicOAuthAvailable()
		m.setFlash(why, true)
		return m.flashCmd()
	}
	m.overlay = overlayNone
	return runLogin(k)
}

// saveOAuth records that this provider authenticates by login.
func (m *App) saveOAuth(k provider.Kind) tea.Cmd {
	_, why := provider.AnthropicOAuthAvailable()
	pc := m.cfg.Providers[k.Type]
	pc.Type = k.Type
	pc.Auth = "oauth"
	// A stored key would outrank the profile and make the login look like it
	// did nothing, so connecting by login clears it.
	pc.APIKey = ""
	pc.APIKeyEnv = ""
	if m.cfg.Providers == nil {
		m.cfg.Providers = map[string]config.ProviderConfig{}
	}
	m.cfg.Providers[k.Type] = pc
	m.saveCfg()

	m.setFlash(k.Label+" connected by login"+gl.sep+why, false)
	return m.flashCmd()
}
