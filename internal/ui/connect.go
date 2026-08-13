package ui

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"tapioca/internal/config"
	"tapioca/internal/provider"
)

// Connecting a provider meant editing config.toml, and nothing on screen said
// which providers existed, which were configured, or why one was failing. This
// is that screen: every provider Tapioca can reach, with a status that comes
// from an actual call rather than from the presence of a key.

// connState is what a provider is doing, in the order the list sorts by.
type connState int

const (
	connReady   connState = iota // credentials work; a real call succeeded
	connFailing                  // configured, but the call failed
	connUnset                    // nothing configured
)

type connEntry struct {
	kind   provider.Kind
	name   string // the config key, when configured
	state  connState
	detail string // model count, or the actual error
}

// connStatusMsg carries the probe results back to the update loop.
type connStatusMsg struct{ entries []connEntry }

// connProbeTimeout is per provider, not for the sweep: one unreachable
// endpoint must not decide how long the screen takes to appear.
const connProbeTimeout = 5 * time.Second

// probeConnections asks every catalog entry whether it works, concurrently.
// Presence of a key proves nothing — a revoked key is present and useless — so
// each configured provider gets a real ListModels call.
func probeConnections(cfg *config.Config) tea.Cmd {
	// Config entries by the catalog type they belong to, so a provider named
	// something other than its type is still recognised.
	byType := map[string][]string{}
	for name, pc := range cfg.Providers {
		if k, ok := provider.KindFor(pc.Type); ok {
			byType[k.Type] = append(byType[k.Type], name)
		}
	}
	for _, names := range byType {
		sort.Strings(names)
	}
	configs := map[string]config.ProviderConfig{}
	for name, pc := range cfg.Providers {
		configs[name] = pc
	}

	return func() tea.Msg {
		entries := make([]connEntry, len(provider.Catalog))
		var wg sync.WaitGroup
		for i, k := range provider.Catalog {
			names := byType[k.Type]
			if len(names) == 0 {
				entries[i] = connEntry{kind: k, state: connUnset, detail: "not configured"}
				continue
			}
			wg.Add(1)
			go func(i int, k provider.Kind, name string) {
				defer wg.Done()
				entries[i] = probeOne(k, name, configs[name])
			}(i, k, names[0])
		}
		wg.Wait()
		return connStatusMsg{entries: entries}
	}
}

func probeOne(k provider.Kind, name string, pc config.ProviderConfig) connEntry {
	e := connEntry{kind: k, name: name}
	p, err := provider.New(name, pc)
	if err != nil {
		e.state, e.detail = connFailing, err.Error()
		return e
	}
	ctx, cancel := context.WithTimeout(context.Background(), connProbeTimeout)
	defer cancel()
	models, err := p.ListModels(ctx)
	if err != nil {
		e.state, e.detail = connFailing, err.Error()
		return e
	}
	e.state = connReady
	switch len(models) {
	case 0:
		// Reachable but offering nothing is not success: picking it would
		// leave the user with an empty model list and no explanation.
		e.state, e.detail = connFailing, "connected, but no models available"
	case 1:
		e.detail = "ready" + gl.sep + "1 model"
	default:
		e.detail = fmt.Sprintf("ready"+gl.sep+"%d models", len(models))
	}
	return e
}

// mark is the at-a-glance status glyph. The ascii set has no colour and no
// symbols, so these have to read as status on their own.
func (e connEntry) mark() string {
	switch e.state {
	case connReady:
		return gl.toolOK
	case connFailing:
		return gl.toolErr
	default:
		return " "
	}
}

// openConnectPicker shows the probed providers, ready first and unconfigured
// last, so what works is visible without scrolling and what needs attention is
// next to it.
func (m *App) openConnectPicker(entries []connEntry) {
	sorted := append([]connEntry(nil), entries...)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].state < sorted[j].state })

	items := make([]pickerItem, 0, len(sorted))
	for _, e := range sorted {
		label := e.mark() + " " + e.kind.Label
		desc := e.detail
		if e.state == connUnset && e.kind.Desc != "" {
			desc = e.kind.Desc
		}
		items = append(items, pickerItem{
			label:  label,
			desc:   desc,
			value:  e.kind.Type,
			search: e.kind.Label + " " + e.kind.Desc,
		})
	}
	m.conn = sorted
	m.pick = newPicker(pickConnect, "providers", items)
	m.overlay = overlayPicker
}

// connEntryFor returns the probed entry for a catalog type.
func (m *App) connEntryFor(typ string) (connEntry, bool) {
	for _, e := range m.conn {
		if e.kind.Type == typ {
			return e, true
		}
	}
	return connEntry{}, false
}

// applyConnect acts on the selected provider. Each state has a different
// useful answer, and none of them is closing the screen in silence.
func (m *App) applyConnect(typ string) tea.Cmd {
	e, ok := m.connEntryFor(typ)
	if !ok {
		return nil
	}
	if e.state == connReady {
		return m.useProvider(e.name)
	}

	// Failing and unconfigured take the same path. The default config ships an
	// anthropic entry, so that provider is never "unconfigured" — it is
	// configured and broken until a key exists, and reporting the error again
	// is no use: the list already showed it, and selecting a broken provider
	// means "let me fix this". Reporting without offering a fix is the dead
	// end this whole screen exists to remove.
	switch {
	case len(e.kind.Fields) == 1 || singleSecret(e.kind):
		m.openCredentialEntry(e.kind)
	default:
		// Providers needing several fields are #137; until then, say what they
		// need rather than opening a form that can only ask for one of them.
		m.setFlash(e.kind.Needs(), false)
		return m.flashCmd()
	}
	return nil
}

// useProvider points the active agent at a provider that is known to work.
// The model is left alone: this switches the account, not the choice of model,
// and /model is the place to change that.
func (m *App) useProvider(name string) tea.Cmd {
	a := m.mgr.ActiveAgent()
	if a == nil {
		return nil
	}
	p, err := m.mgr.ProviderFor(name)
	if err != nil {
		m.setFlash(err.Error(), true)
		return m.flashCmd()
	}
	a.Provider, a.ProviderName, a.ProviderErr = p, name, ""
	m.cfg.DefaultProvider = name
	m.saveCfg()
	m.setFlash("using "+name, false)
	return m.flashCmd()
}

// singleSecret reports whether a provider needs exactly one required value,
// which is the shape the entry flow handles.
func singleSecret(k provider.Kind) bool {
	n := 0
	for _, f := range k.Fields {
		if !f.Optional {
			n++
		}
	}
	return n == 1
}

// cmdConnect starts the probe. It reports progress first because the sweep
// takes as long as the slowest provider, and a screen that appears to hang is
// worse than one that says what it is doing.
func cmdConnect(m *App, _ string) tea.Cmd {
	m.setFlash("checking providers…", false)
	return tea.Batch(probeConnections(m.cfg), m.flashCmd())
}
