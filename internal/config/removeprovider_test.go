package config

import "testing"

// A provider deleted from the file is gone. Decoding a table into a map merges
// into it, so the built-in entries used to survive their own deletion: there
// was no edit to config.toml that removed ollama, and it stayed in /connect and
// in the sweep /model does over every configured provider.
func TestDeletingAProviderRemovesIt(t *testing.T) {
	cfg, err := Load(writeConfig(t, `
[providers]
  [providers.llamacpp]
    type = "llamacpp"
    base_url = "http://localhost:8080"
`))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := cfg.Providers["ollama"]; ok {
		t.Error("ollama is still configured after being deleted from the file")
	}
	if _, ok := cfg.Providers["anthropic"]; ok {
		t.Error("anthropic is still configured after being deleted from the file")
	}
	if _, ok := cfg.Providers["llamacpp"]; !ok {
		t.Fatal("the provider the file does name is missing")
	}
	if len(cfg.Providers) != 1 {
		t.Errorf("providers = %v, want just the one the file names", cfg.Providers)
	}
}

// The other half of that: clearing the map before decoding must not cost a
// first-run config its providers. A file that never mentions them still gets
// the defaults — this is what the clear could plausibly have broken.
func TestAFileWithNoProvidersKeepsTheDefaults(t *testing.T) {
	cfg, err := Load(writeConfig(t, "theme = \"taro\"\n"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"ollama", "anthropic"} {
		if _, ok := cfg.Providers[want]; !ok {
			t.Errorf("default provider %q missing from a config that never mentioned providers", want)
		}
	}
}

// default_provider names "ollama" without the user having written it down, so
// deleting the ollama entry and nothing else would have left every new agent
// pointed at a provider that is not there.
func TestTheDefaultProviderFollowsTheOneThatIsLeft(t *testing.T) {
	cfg, err := Load(writeConfig(t, `
[providers]
  [providers.llamacpp]
    type = "llamacpp"
    base_url = "http://localhost:8080"
`))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := cfg.Providers[cfg.DefaultProvider]; !ok {
		t.Errorf("default_provider = %q, which is not a configured provider (%v)",
			cfg.DefaultProvider, cfg.Providers)
	}
}

// But a default the user did choose is left alone.
func TestAChosenDefaultProviderIsKept(t *testing.T) {
	cfg, err := Load(writeConfig(t, `
default_provider = "ollama"
[providers]
  [providers.anthropic]
    type = "anthropic"
  [providers.ollama]
    type = "ollama"
`))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DefaultProvider != "ollama" {
		t.Errorf("default_provider = %q, want the configured %q", cfg.DefaultProvider, "ollama")
	}
}
