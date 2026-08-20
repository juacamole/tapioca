package ui

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	tea "charm.land/bubbletea/v2"

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
	// external is set when the entry is another agent to drive rather than a
	// provider to send to. They share this screen because they answer the same
	// question: what can this session talk to?
	external *config.ExternalAgent
	// detected marks a server found listening that no config mentions, so
	// choosing it writes the entry rather than switching to one already there.
	detected bool
}

// connStatusMsg carries the probe results back to the update loop.
type connStatusMsg struct{ entries []connEntry }

// connProbeTimeout is per provider, not for the sweep: one unreachable
// endpoint must not decide how long the screen takes to appear.
const connProbeTimeout = 5 * time.Second

// probeConnections asks every catalog entry whether it works, concurrently.
// Presence of a key proves nothing — a revoked key is present and useless — so
// each configured provider gets a real ListModels call. Configured external
// agents join the results: the screen is what this session can talk to.
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
			if len(names) == 0 && !k.Detectable() {
				entries[i] = connEntry{kind: k, state: connUnset, detail: "not configured"}
				continue
			}
			wg.Add(1)
			go func(i int, k provider.Kind, names []string) {
				defer wg.Done()
				if len(names) == 0 {
					entries[i] = detectOne(k)
					return
				}
				entries[i] = probeOne(k, names[0], configs[names[0]])
			}(i, k, names)
		}
		wg.Wait()
		return connStatusMsg{entries: append(entries, externalEntries(cfg)...)}
	}
}

// detectOne looks for a local server nobody has written into the config yet.
// A llama-server on its usual port is either listening or it is not, and
// answering that question costs one connection to this machine — cheaper than
// making the user describe it in a file first, and the answer is the same.
//
// Not finding one is reported as "not configured" rather than as a failure:
// nothing is wrong with a machine that is not running a local model server,
// and a red mark against it would say there was.
func detectOne(k provider.Kind) connEntry {
	pc := config.ProviderConfig{Type: k.Type, BaseURL: k.DefaultAddress()}
	e := probeOne(k, k.Type, pc)
	if e.state != connReady {
		return connEntry{kind: k, state: connUnset, detail: "not configured"}
	}
	// Named so that choosing it is obviously an addition, not a switch to
	// something already set up.
	e.detail += gl.sep + "found on " + k.DefaultAddress()
	e.detected = true
	return e
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
		if e.external != nil {
			items = append(items, pickerItem{
				label:  e.mark() + " " + e.external.Name,
				desc:   e.detail,
				value:  externalPick + e.external.Name,
				search: e.external.Name + " agent acp " + e.external.Command,
			})
			continue
		}
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
	title := "providers"
	for _, e := range sorted {
		if e.external != nil {
			title = "providers and agents"
			break
		}
	}
	m.pick = newPicker(pickConnect, title, items)
	m.overlay = overlayPicker
}

// connEntryFor returns the probed entry for a catalog type.
func (m *App) connEntryFor(typ string) (connEntry, bool) {
	for _, e := range m.conn {
		if e.external == nil && e.kind.Type == typ {
			return e, true
		}
	}
	return connEntry{}, false
}

// applyConnect acts on the selection, which is a provider or an external
// agent. Each state has a different useful answer, and none of them is closing
// the screen in silence.
func (m *App) applyConnect(typ string) tea.Cmd {
	if name, ok := strings.CutPrefix(typ, externalPick); ok {
		return m.connectExternal(name)
	}
	e, ok := m.connEntryFor(typ)
	if !ok {
		return nil
	}
	if e.state == connReady {
		if e.detected {
			return m.addDetected(e)
		}
		return m.useProvider(e.name)
	}

	// Failing and unconfigured take the same path. The default config ships an
	// anthropic entry, so that provider is never "unconfigured" — it is
	// configured and broken until a key exists, and reporting the error again
	// is no use: the list already showed it, and selecting a broken provider
	// means "let me fix this". Reporting without offering a fix is the dead
	// end this whole screen exists to remove.
	// The form asks for as many fields as the provider declares, so every
	// provider goes through it. A provider with nothing to configure has an
	// empty field list and would open an empty form, which is the one case
	// worth reporting instead.
	if len(e.kind.Fields) == 0 {
		m.setFlash(e.kind.Needs(), false)
		return m.flashCmd()
	}
	m.openCredentialEntry(e.kind)
	return nil
}

// addDetected writes the entry for a server that was found listening and then
// uses it. The address is the one it answered on, so there is nothing left to
// ask: a form whose every field already has the right value is a form that
// only exists to be dismissed.
func (m *App) addDetected(e connEntry) tea.Cmd {
	name := m.addProviderEntry(e.kind)
	m.saveCfg()
	m.mgr.ReloadProviders()
	return m.useProvider(name)
}

// addProviderEntry writes a found server into the config and returns the name
// it was written under.
func (m *App) addProviderEntry(k provider.Kind) string {
	if m.cfg.Providers == nil {
		m.cfg.Providers = map[string]config.ProviderConfig{}
	}
	// A name already in use belongs to something else the user set up, and
	// taking it would silently repoint their provider at this one.
	name := k.Type
	for _, taken := m.cfg.Providers[name]; taken; _, taken = m.cfg.Providers[name] {
		name += "-local"
	}
	m.cfg.Providers[name] = config.ProviderConfig{Type: k.Type, BaseURL: k.DefaultAddress()}
	return name
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

// cmdConnect starts the probe. It reports progress first because the sweep
// takes as long as the slowest provider, and a screen that appears to hang is
// worse than one that says what it is doing. A named external agent skips the
// screen: the user already said what they wanted.
func cmdConnect(m *App, arg string) tea.Cmd {
	if arg != "" {
		return m.connectExternal(arg)
	}
	m.setFlash("checking providers…", false)
	return tea.Batch(probeConnections(m.cfg), m.flashCmd())
}
