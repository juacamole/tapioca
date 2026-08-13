package ui

import (
	"errors"
	"fmt"

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
