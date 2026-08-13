package ui

import (
	"testing"

	"tapioca/internal/agent"
	"tapioca/internal/config"
)

// A gateway fronts models from every vendor, so its context window cannot be
// guessed from the provider type — it has to come from the model. When the type
// switch had no case for a gateway it returned the 8192 fallback, and
// compactThreshold takes 80% of that: a 200k model reached through a gateway
// started compacting at ~6.5k tokens, roughly 3% of its real context.
func TestGatewayContextWindowIsNotTheTinyFallback(t *testing.T) {
	for _, typ := range []string{"vercel", "custom"} {
		m := &App{cfg: config.Default(), w: 100, h: 30}
		m.cfg.Providers["gw"] = config.ProviderConfig{Type: typ}
		a := &agent.Agent{ProviderName: "gw", Model: "anthropic/claude-opus-5"}

		got := m.contextWindowFor(a)
		if got <= 8192 {
			t.Errorf("%s: contextWindowFor = %d; compaction would start at %d tokens",
				typ, got, compactThreshold(got))
		}
	}
}

// An explicit context_window in the config is the user telling us the answer,
// and it has to keep winning over anything inferred.
func TestConfiguredContextWindowStillWins(t *testing.T) {
	m := &App{cfg: config.Default(), w: 100, h: 30}
	m.cfg.Providers["gw"] = config.ProviderConfig{Type: "vercel", ContextWindow: 42_000}
	a := &agent.Agent{ProviderName: "gw", Model: "anthropic/claude-opus-5"}

	if got := m.contextWindowFor(a); got != 42_000 {
		t.Errorf("contextWindowFor = %d, want the configured 42000", got)
	}
}

// Local models must not pick up hosted pricing just because their name happens
// to carry a slash — ollama tags like "library/qwen" are not gateway ids.
func TestLocalModelsAreStillFree(t *testing.T) {
	m := &App{cfg: config.Default(), w: 100, h: 30}
	m.cfg.Providers["ollama"] = config.ProviderConfig{Type: "ollama"}

	if _, ok := m.costFor("ollama", "library/qwen3"); ok {
		t.Error("a local model was given a price")
	}
}
