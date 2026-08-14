package ui

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	tea "charm.land/bubbletea/v2"

	"tapioca/internal/agent"
	"tapioca/internal/provider"
)

// startSpawn runs a delegated task in a fresh agent tab. The waiting parent is
// recorded by subagent id so its answer can be handed back when the turn ends.
func (m *App) startSpawn(parentID int, req *agent.SpawnReq) tea.Cmd {
	parent := m.mgr.ByID(parentID)
	if parent == nil {
		req.Reply <- agent.SpawnResult{Err: errors.New("the delegating agent is gone")}
		return nil
	}
	na := m.mgr.Spawn(parent, req.Name)
	// A requested model is resolved before anything is sent. Failing here
	// means the parent gets a usable error; letting it through would run the
	// task on the parent's model while the tab claimed otherwise, which is
	// worse than refusing.
	if req.Model != "" {
		if err := m.applySpawnModel(na, parent, req.Model); err != nil {
			m.mgr.Close(m.indexOf(na.ID))
			req.Reply <- agent.SpawnResult{Err: err}
			return nil
		}
	}
	if na.Provider == nil {
		err := errors.New("no provider configured for the subagent")
		m.mgr.Close(m.indexOf(na.ID))
		req.Reply <- agent.SpawnResult{Err: err}
		return nil
	}
	m.spawns[na.ID] = req

	task := provider.TextMessage("user", req.Task)
	na.Messages = append(na.Messages, task)
	na.Status = agent.StatusWaiting
	na.StatusDetail = "sending request"
	na.Send(append([]provider.Message(nil), na.Messages...))

	m.dirty = true
	m.setFlash(fmt.Sprintf("%s delegated to %s", parent.Name, na.Name), false)
	return tea.Batch(waitAgent(na), m.flashCmd())
}

// applySpawnModel points a subagent at a requested "[provider:]model". Only
// the provider is checked here: whether a model name exists is a question only
// the provider can answer, and asking would mean a network round trip inside a
// tool call. A wrong name surfaces as that provider's own error on the first
// request, which names the problem better than a guess would.
func (m *App) applySpawnModel(na, parent *agent.Agent, ref string) error {
	provName, model := m.splitModelRef(ref, parent.ProviderName)
	// Staying on the parent's provider reuses the instance it is already
	// using rather than building a second one from config. Rebuilding would
	// fail whenever the parent is working from a credential the config cannot
	// reproduce — which is the common case for "same provider, cheaper model".
	p := parent.Provider
	if provName != parent.ProviderName || p == nil {
		var err error
		if p, err = m.mgr.ProviderFor(provName); err != nil {
			return fmt.Errorf("%s — configured providers: %s", err, strings.Join(m.providerNames(), ", "))
		}
	}
	na.Provider, na.ProviderName, na.ProviderErr, na.Model = p, provName, "", model
	return nil
}

// providerNames lists the configured providers, for an error that says what
// could have been asked for instead.
func (m *App) providerNames() []string {
	names := make([]string, 0, len(m.cfg.Providers))
	for name := range m.cfg.Providers {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// finishSpawn hands a finished subagent's answer to the parent waiting on it.
func (m *App) finishSpawn(a *agent.Agent) {
	req, waiting := m.spawns[a.ID]
	if !waiting {
		return
	}
	delete(m.spawns, a.ID)
	if a.LastErr != "" {
		req.Reply <- agent.SpawnResult{Err: errors.New(a.LastErr)}
		return
	}
	req.Reply <- agent.SpawnResult{Text: lastAssistantText(a)}
}

// releaseSpawn unblocks a parent whose subagent is being closed or discarded,
// so a delegating agent can never be left waiting on a tab that no longer runs.
func (m *App) releaseSpawn(id int, reason string) {
	if req, waiting := m.spawns[id]; waiting {
		delete(m.spawns, id)
		req.Reply <- agent.SpawnResult{Err: errors.New(reason)}
	}
}

// releaseAllSpawns unblocks every waiting parent, for wholesale replacements
// of the workspace (/new, /resume).
func (m *App) releaseAllSpawns(reason string) {
	for id := range m.spawns {
		m.releaseSpawn(id, reason)
	}
}

func (m *App) indexOf(id int) int {
	for i, a := range m.mgr.Agents {
		if a.ID == id {
			return i
		}
	}
	return -1
}

func lastAssistantText(a *agent.Agent) string {
	for i := len(a.Messages) - 1; i >= 0; i-- {
		if a.Messages[i].Role == "assistant" {
			if t := a.Messages[i].Text(); t != "" {
				return t
			}
		}
	}
	return ""
}
