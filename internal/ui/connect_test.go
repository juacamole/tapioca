package ui

import (
	"strings"
	"testing"

	"tapioca/internal/config"
	"tapioca/internal/provider"
)

// The reported bug: with nothing reachable, /model flashed an error that
// expired and left nothing to act on. An empty model list has to lead
// somewhere, so the handler must return work rather than a message.
func TestEmptyModelListDoesNotDeadEnd(t *testing.T) {
	m := &App{cfg: config.Default(), w: 100, h: 30}
	_, cmd := m.Update(modelsLoadedMsg{items: nil, errs: []string{"ollama: dial refused"}})
	if cmd == nil {
		t.Fatal("an empty model list produced no follow-up — this is the dead end")
	}
}

// The destination: probe results open the connect picker, not a flash.
func TestProbeResultsOpenTheConnectPicker(t *testing.T) {
	m := &App{cfg: config.Default(), w: 100, h: 30}
	anthropic, _ := provider.KindFor("anthropic")
	m.Update(connStatusMsg{entries: []connEntry{
		{kind: anthropic, state: connUnset, detail: "not configured"},
	}})
	if m.overlay != overlayPicker {
		t.Fatalf("overlay = %v, want the picker", m.overlay)
	}
	if m.pick.kind != pickConnect {
		t.Errorf("picker kind = %v, want pickConnect", m.pick.kind)
	}
}

// Errors used to be joined into one string, which meant the reason a specific
// provider failed was buried in a line about all of them. Each entry keeps its
// own.
func TestEachProviderKeepsItsOwnError(t *testing.T) {
	ollama, _ := provider.KindFor("ollama")
	anthropic, _ := provider.KindFor("anthropic")
	m := &App{cfg: config.Default(), w: 100, h: 30}
	m.openConnectPicker([]connEntry{
		{kind: ollama, state: connFailing, detail: "dial tcp 127.0.0.1:1: refused"},
		{kind: anthropic, state: connFailing, detail: "no API key"},
	})

	var sawOllama, sawAnthropic bool
	for _, it := range m.pick.items {
		switch {
		case strings.Contains(it.label, "ollama"):
			sawOllama = true
			if !strings.Contains(it.desc, "refused") {
				t.Errorf("ollama entry lost its own error: %q", it.desc)
			}
			if strings.Contains(it.desc, "no API key") {
				t.Errorf("ollama entry carries anthropic's error: %q", it.desc)
			}
		case strings.Contains(it.label, "anthropic"):
			sawAnthropic = true
			if !strings.Contains(it.desc, "no API key") {
				t.Errorf("anthropic entry lost its own error: %q", it.desc)
			}
		}
	}
	if !sawOllama || !sawAnthropic {
		t.Fatalf("expected both providers in the list, got %d items", len(m.pick.items))
	}
}

// What works belongs at the top: the list exists to be acted on, and a user
// with one working provider among seven should not hunt for it.
func TestReadyProvidersSortFirst(t *testing.T) {
	ollama, _ := provider.KindFor("ollama")
	anthropic, _ := provider.KindFor("anthropic")
	openai, _ := provider.KindFor("openai")
	m := &App{cfg: config.Default(), w: 100, h: 30}
	m.openConnectPicker([]connEntry{
		{kind: openai, state: connUnset},
		{kind: anthropic, state: connFailing, detail: "no API key"},
		{kind: ollama, state: connReady, detail: "ready"},
	})
	want := []string{"ollama", "anthropic", "openai"}
	for i, w := range want {
		if !strings.Contains(m.pick.items[i].label, w) {
			t.Errorf("position %d is %q, want %s (ready, then failing, then unconfigured)",
				i, m.pick.items[i].label, w)
		}
	}
}

// A provider that answers but offers nothing is not ready: selecting it would
// hand the user an empty model list with no explanation.
func TestReachableButEmptyCountsAsFailing(t *testing.T) {
	k, _ := provider.KindFor("ollama")
	e := probeOne(k, "ollama", config.ProviderConfig{Type: "ollama", BaseURL: "http://127.0.0.1:1"})
	if e.state != connFailing {
		t.Errorf("unreachable provider reported state %v, want failing", e.state)
	}
	if e.detail == "" {
		t.Error("a failing provider must say why")
	}
}
