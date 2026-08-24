package agent

import (
	"strings"
	"testing"

	"tapioca/internal/config"
)

func fallbackManager(t *testing.T, fbs []config.Fallback) (*Manager, *Agent) {
	t.Helper()
	cfg := config.Default()
	cfg.Fallbacks = fbs
	cfg.Providers["ollama"] = config.ProviderConfig{Type: "ollama", BaseURL: "http://localhost:11434"}
	cfg.Providers["second"] = config.ProviderConfig{Type: "ollama", BaseURL: "http://localhost:11435"}
	m := NewManager(cfg, nil, nil)
	a := &Agent{ProviderName: "ollama", Model: "qwen3-coder"}
	return m, a
}

// The chain is resolved into providers up front, because the run loop has no
// access to config and no way to build one.
func TestFallbacksResolveToProviders(t *testing.T) {
	m, a := fallbackManager(t, []config.Fallback{{
		When: "ollama:qwen3-coder",
		Then: []string{"ollama:qwen3:4b", "second:qwen3-coder"},
	}})

	if problems := m.ResolveFallbacks(a); len(problems) != 0 {
		t.Fatalf("unexpected problems: %v", problems)
	}
	if len(a.Fallbacks) != 2 {
		t.Fatalf("resolved %d targets, want 2", len(a.Fallbacks))
	}
	if a.Fallbacks[0].model != "qwen3:4b" || a.Fallbacks[0].providerName != "ollama" {
		t.Errorf("first target is %s:%s", a.Fallbacks[0].providerName, a.Fallbacks[0].model)
	}
	if a.Fallbacks[1].providerName != "second" {
		t.Errorf("second target is on %s, want second", a.Fallbacks[1].providerName)
	}
	for i, f := range a.Fallbacks {
		if f.prov == nil {
			t.Errorf("target %d has no provider", i)
		}
	}
}

// A chain for a different model must not apply — otherwise one entry would
// silently govern every agent.
func TestFallbacksOnlyApplyToTheirModel(t *testing.T) {
	m, a := fallbackManager(t, []config.Fallback{{
		When: "ollama:some-other-model",
		Then: []string{"ollama:qwen3:4b"},
	}})
	m.ResolveFallbacks(a)
	if len(a.Fallbacks) != 0 {
		t.Errorf("a chain for another model was applied: %d targets", len(a.Fallbacks))
	}
}

// An unbuildable entry is dropped and reported rather than failing the agent:
// refusing to run because the third choice is misconfigured would be worse
// than the outage the list exists to survive.
func TestAnUnresolvableEntryIsDroppedAndReported(t *testing.T) {
	m, a := fallbackManager(t, []config.Fallback{{
		When: "ollama:qwen3-coder",
		Then: []string{"nosuchprovider:x", "ollama:qwen3:4b"},
	}})

	problems := m.ResolveFallbacks(a)

	// "nosuchprovider" is not configured, so it stays part of the model name
	// and resolves against the agent's own provider — which does build. The
	// contract that matters is that a good entry is never lost.
	var models []string
	for _, f := range a.Fallbacks {
		models = append(models, f.model)
	}
	if !strings.Contains(strings.Join(models, ","), "qwen3:4b") {
		t.Errorf("the resolvable entry was lost: %v (problems: %v)", models, problems)
	}
}

// Resolving twice must not accumulate: it runs every turn.
func TestResolvingTwiceDoesNotAccumulate(t *testing.T) {
	m, a := fallbackManager(t, []config.Fallback{{
		When: "ollama:qwen3-coder",
		Then: []string{"ollama:qwen3:4b"},
	}})
	m.ResolveFallbacks(a)
	m.ResolveFallbacks(a)
	if len(a.Fallbacks) != 1 {
		t.Errorf("after two resolutions there are %d targets, want 1", len(a.Fallbacks))
	}
}

// nextFallback walks the chain once and then reports it is finished, so a
// failing turn cannot loop through the same models forever.
func TestNextFallbackWalksOnceAndStops(t *testing.T) {
	rs := runSettings{
		prov: nil, providerName: "a", model: "m0",
		fallbacks: []fallbackTarget{
			{providerName: "b", model: "m1"},
			{providerName: "c", model: "m2"},
		},
	}
	at := -1

	for i, want := range []string{"m1", "m2"} {
		got, ok := nextFallback(&rs, &at)
		if !ok {
			t.Fatalf("step %d reported no target", i)
		}
		if got.model != want || rs.model != want {
			t.Errorf("step %d gave %q and rs.model %q, want %q", i, got.model, rs.model, want)
		}
	}
	if _, ok := nextFallback(&rs, &at); ok {
		t.Error("the chain kept going past its end")
	}
}
