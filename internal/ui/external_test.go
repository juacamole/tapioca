package ui

import (
	"strings"
	"testing"

	"tapioca/internal/agent"
	"tapioca/internal/config"
	"tapioca/internal/provider"
	"tapioca/internal/tools"
)

func cfgWithExternal(agents ...config.ExternalAgent) *config.Config {
	cfg := config.Default()
	cfg.Agents.External = agents
	return cfg
}

// The connect screen answers "what can this session talk to", and another
// agent is one of the answers.
func TestExternalAgentsAppearOnTheConnectScreen(t *testing.T) {
	cfg := cfgWithExternal(
		config.ExternalAgent{Name: "on-path", Command: "go"},
		config.ExternalAgent{Name: "missing", Command: "tapioca-not-a-real-command"},
		config.ExternalAgent{Name: "empty"},
	)
	entries := externalEntries(cfg)
	if len(entries) != 3 {
		t.Fatalf("got %d entries, want one per configured agent", len(entries))
	}
	states := map[string]connState{}
	for _, e := range entries {
		states[e.name] = e.state
		if e.detail == "" {
			t.Errorf("%s says nothing about its state", e.name)
		}
	}
	if states["on-path"] != connReady {
		t.Errorf("an agent whose command exists reported %v", states["on-path"])
	}
	if states["missing"] != connFailing {
		t.Errorf("an agent whose command is not on PATH reported %v", states["missing"])
	}
	if states["empty"] != connUnset {
		t.Errorf("an agent with no command reported %v", states["empty"])
	}
}

// The picker mixes agents and providers, so a selection has to say which it
// was: an agent named like a provider must not open a credential form.
func TestSelectingAnExternalAgentConnectsIt(t *testing.T) {
	cfg := cfgWithExternal(config.ExternalAgent{Name: "anthropic", Command: "go"})
	mgr := agent.NewManager(cfg, nil, tools.NewExecutor(t.TempDir(), tools.ModeManual))
	mgr.NewAgent()
	m := &App{cfg: cfg, mgr: mgr, w: 100, h: 30}
	anthropic, _ := provider.KindFor("anthropic")
	m.openConnectPicker([]connEntry{
		{kind: anthropic, name: "anthropic", state: connUnset},
		{name: "anthropic", state: connReady, external: &m.cfg.Agents.External[0]},
	})

	var value string
	for _, it := range m.pick.items {
		if strings.HasPrefix(it.value, externalPick) {
			value = it.value
		}
	}
	if value == "" {
		t.Fatal("the external agent is not selectable from the connect screen")
	}
	if cmd := m.applyConnect(value); cmd == nil {
		t.Fatal("selecting an external agent did nothing")
	}
	if m.overlay == overlayCredential {
		t.Error("selecting an agent opened the provider credential form")
	}
}

func TestConnectingAnUnknownAgentSaysSo(t *testing.T) {
	m := &App{cfg: config.Default(), w: 100, h: 30}
	m.connectExternal("nope")
	if !strings.Contains(m.flash, "nope") || !m.flashErr {
		t.Errorf("flash = %q (err=%v), want an error naming the agent", m.flash, m.flashErr)
	}
}

func TestConnectedAgentGetsATabAndAFailureDoesNot(t *testing.T) {
	mgr := agent.NewManager(config.Default(), nil, nil)
	mgr.NewAgent()
	m := &App{cfg: config.Default(), mgr: mgr, w: 100, h: 30, dashScroll: map[string]int{}}

	m.handleExternalConnected(externalConnectedMsg{name: "ext", err: errFailedDial})
	if len(mgr.Agents) != 1 {
		t.Fatalf("a failed connection left %d tabs behind", len(mgr.Agents))
	}
	if !m.flashErr {
		t.Error("a failed connection was not reported")
	}
}

var errFailedDial = errDial("ext exited: status 1")

type errDial string

func (e errDial) Error() string { return string(e) }
