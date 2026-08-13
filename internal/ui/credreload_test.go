package ui

import (
	"testing"

	"tapioca/internal/agent"
	"tapioca/internal/config"
	"tapioca/internal/provider"
)

// Provider instances are cached by name, so one built before a key existed
// keeps being handed out after the key is saved: the screen says "connected"
// and the very next prompt fails with the credential the provider was born
// with. Saving a credential is a config edit, and the manager already has the
// function for that.
func TestConnectingRebuildsTheCachedProvider(t *testing.T) {
	cfg := config.Default()
	cfg.Providers["vercel"] = config.ProviderConfig{Type: "vercel"} // no key yet

	mgr := agent.NewManager(cfg, nil, nil)
	a := &agent.Agent{ProviderName: "vercel"}
	mgr.Agents = []*agent.Agent{a}

	stale, err := mgr.ProviderFor("vercel")
	if err != nil {
		t.Fatal(err)
	}
	a.Provider = stale

	m := &App{cfg: cfg, mgr: mgr, w: 100, h: 30}
	k, _ := provider.KindFor("vercel")
	m.cred = &credForm{kind: k, fields: k.Fields, values: map[string]string{"api_key": "vk-now-configured"}}
	m.handleCredentialTested(credTestedMsg{kind: "vercel", models: 3})

	if cfg.Providers["vercel"].APIKey != "vk-now-configured" {
		t.Fatalf("the key was not saved: %q", cfg.Providers["vercel"].APIKey)
	}
	fresh, err := mgr.ProviderFor("vercel")
	if err != nil {
		t.Fatal(err)
	}
	if fresh == stale {
		t.Error("the provider built before the key was saved is still cached — the next prompt uses the old credential")
	}
}
