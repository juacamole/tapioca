package ui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"tapioca/internal/agent"
	"tapioca/internal/config"
	"tapioca/internal/provider"
)

var ctrlD = tea.KeyPressMsg{Code: 'd', Mod: tea.ModCtrl}

// connectScreen opens the connect picker over a config holding exactly the
// named providers, with the first one selected.
func connectScreen(t *testing.T, entries ...connEntry) (*App, *config.Config) {
	t.Helper()
	cfg := config.Default()
	cfg.Providers = map[string]config.ProviderConfig{
		"ollama":   {Type: "ollama", BaseURL: "http://localhost:11434"},
		"llamacpp": {Type: "llamacpp", BaseURL: "http://localhost:8080"},
	}
	cfg.DefaultProvider = "ollama"
	m := &App{cfg: cfg, mgr: agent.NewManager(cfg, nil, nil), keys: NewKeyMap(cfg.Keys), w: 100, h: 30}
	m.openConnectPicker(entries)
	return m, cfg
}

func kind(t *testing.T, typ string) provider.Kind {
	t.Helper()
	k, ok := provider.KindFor(typ)
	if !ok {
		t.Fatalf("no catalog kind for %q", typ)
	}
	return k
}

// selectValue points the picker at the item carrying this value, so a test
// says which provider it means rather than depending on the sort order.
func selectValue(t *testing.T, m *App, value string) {
	t.Helper()
	for i, it := range m.pick.visible() {
		if it.value == value {
			m.pick.sel = i
			return
		}
	}
	t.Fatalf("no item with value %q on screen", value)
}

// Connecting a provider has been possible from this screen since it existed;
// undoing it meant editing config.toml by hand. The first press arms and
// removes nothing — deleting an entry can destroy a key that lives only in
// that file.
func TestFirstCtrlDArmsWithoutRemoving(t *testing.T) {
	m, cfg := connectScreen(t,
		connEntry{kind: kind(t, "ollama"), name: "ollama", state: connReady, detail: "ready"},
	)
	selectValue(t, m, "ollama")
	m.Update(ctrlD)

	if _, ok := cfg.Providers["ollama"]; !ok {
		t.Error("one press removed the provider; it must arm first")
	}
	if m.connArmed != "ollama" {
		t.Errorf("connArmed = %q, want the selected provider", m.connArmed)
	}
	if !strings.Contains(m.flash, "ollama") {
		t.Errorf("the arming message does not name what would go: %q", m.flash)
	}
}

func TestSecondCtrlDDisconnects(t *testing.T) {
	m, cfg := connectScreen(t,
		connEntry{kind: kind(t, "ollama"), name: "ollama", state: connReady, detail: "ready"},
	)
	selectValue(t, m, "ollama")
	m.Update(ctrlD)
	m.Update(ctrlD)

	if _, ok := cfg.Providers["ollama"]; ok {
		t.Error("the provider is still configured after two presses")
	}
	if _, ok := cfg.Providers["llamacpp"]; !ok {
		t.Error("disconnecting one provider removed another")
	}
}

// Arming names a provider. A second press that landed on a different one would
// remove something the user was never asked about, so moving disarms.
func TestMovingBetweenPressesDisarms(t *testing.T) {
	m, cfg := connectScreen(t,
		connEntry{kind: kind(t, "ollama"), name: "ollama", state: connReady, detail: "ready"},
		connEntry{kind: kind(t, "llamacpp"), name: "llamacpp", state: connReady, detail: "ready"},
	)
	selectValue(t, m, "ollama")
	m.Update(ctrlD)
	m.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	m.Update(ctrlD)

	if len(cfg.Providers) != 2 {
		t.Fatalf("providers = %v, want both still there — moving must disarm", cfg.Providers)
	}
}

// The case the name check alone does not cover: arm one, move away, come back.
// The arming message has long gone from the screen by then, so a press that
// deleted on arrival would delete without having asked.
func TestReturningToAnArmedProviderRearms(t *testing.T) {
	m, cfg := connectScreen(t,
		connEntry{kind: kind(t, "ollama"), name: "ollama", state: connReady, detail: "ready"},
		connEntry{kind: kind(t, "llamacpp"), name: "llamacpp", state: connReady, detail: "ready"},
	)
	selectValue(t, m, "ollama")
	m.Update(ctrlD)
	m.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	m.Update(tea.KeyPressMsg{Code: tea.KeyUp})
	m.Update(ctrlD)

	if _, ok := cfg.Providers["ollama"]; !ok {
		t.Fatal("deleted on returning to it, without arming again")
	}
	if m.connArmed != "ollama" {
		t.Errorf("connArmed = %q, want it armed afresh", m.connArmed)
	}
}

// A provider with nothing configured has nothing to disconnect, and saying so
// is better than a key that silently does nothing.
func TestDisconnectingAnUnconfiguredProviderSaysSo(t *testing.T) {
	m, cfg := connectScreen(t,
		connEntry{kind: kind(t, "openai"), state: connUnset, detail: "not configured"},
	)
	selectValue(t, m, "openai")
	m.Update(ctrlD)

	if len(cfg.Providers) != 2 {
		t.Errorf("providers changed: %v", cfg.Providers)
	}
	if !m.flashErr || !strings.Contains(m.flash, "nothing to disconnect") {
		t.Errorf("flash = %q (err=%v), want a reason", m.flash, m.flashErr)
	}
}

// default_provider named the entry that just went, and a config saved naming a
// provider that is not there is one every new agent starts broken against.
func TestDisconnectingTheDefaultMovesIt(t *testing.T) {
	m, cfg := connectScreen(t,
		connEntry{kind: kind(t, "ollama"), name: "ollama", state: connReady, detail: "ready"},
	)
	selectValue(t, m, "ollama")
	m.Update(ctrlD)
	m.Update(ctrlD)

	if cfg.DefaultProvider == "ollama" {
		t.Fatal("default_provider still names the disconnected provider")
	}
	if _, ok := cfg.Providers[cfg.DefaultProvider]; !ok {
		t.Errorf("default_provider = %q, which is not configured (%v)", cfg.DefaultProvider, cfg.Providers)
	}
}

// Providers are cached by name once built, so the config edit alone would
// leave the disconnected one working for the rest of the session.
func TestDisconnectDropsTheCachedProvider(t *testing.T) {
	m, cfg := connectScreen(t,
		connEntry{kind: kind(t, "ollama"), name: "ollama", state: connReady, detail: "ready"},
	)
	if _, err := m.mgr.ProviderFor("ollama"); err != nil {
		t.Fatal(err)
	}
	selectValue(t, m, "ollama")
	m.Update(ctrlD)
	m.Update(ctrlD)

	if _, err := m.mgr.ProviderFor("ollama"); err == nil {
		t.Error("the disconnected provider is still handed out from the cache")
	}
	_ = cfg
}

// The list filters on typed text, so a plain d would make "bedrock"
// unreachable by typing. ctrl+d is not a character, and d still filters.
func TestPlainDStillFilters(t *testing.T) {
	m, cfg := connectScreen(t,
		connEntry{kind: kind(t, "ollama"), name: "ollama", state: connReady, detail: "ready"},
	)
	m.Update(tea.KeyPressMsg{Code: 'd', Text: "d"})

	if m.pick.filter != "d" {
		t.Errorf("filter = %q, want the typed character", m.pick.filter)
	}
	if len(cfg.Providers) != 2 {
		t.Errorf("typing a letter changed the config: %v", cfg.Providers)
	}
}

// The key is worth nothing if nobody finds it.
func TestTheHintNamesTheKey(t *testing.T) {
	m, _ := connectScreen(t,
		connEntry{kind: kind(t, "ollama"), name: "ollama", state: connReady, detail: "ready"},
	)
	if v := m.pick.view(100, 30, nil); !strings.Contains(v, "ctrl+d") {
		t.Error("the connect screen does not mention ctrl+d")
	}
}
