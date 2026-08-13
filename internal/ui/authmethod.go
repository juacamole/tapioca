package ui

import (
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

// applyAuthMethod acts on the chosen method.
func (m *App) applyAuthMethod(method string) tea.Cmd {
	k := m.credKind
	if method == authMethodKey {
		m.openCredentialEntry(k)
		return nil
	}

	ok, why := provider.AnthropicOAuthAvailable()
	if !ok {
		// A missing CLI and a missing login are different problems with
		// different fixes, and the reason carries which one it is.
		m.setFlash(why, true)
		return m.flashCmd()
	}

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
