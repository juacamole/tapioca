package agent

import (
	"fmt"
	"time"

	"tapioca/internal/config"
	"tapioca/internal/mcp"
	"tapioca/internal/provider"
	"tapioca/internal/session"
	"tapioca/internal/tools"
)

// Manager owns every agent in the workspace.
type Manager struct {
	Agents []*Agent
	Active int

	cfg    *config.Config
	MCP    *mcp.Registry
	Exec   *tools.Executor
	nextID int
	provs  map[string]provider.Provider // cache by provider name
}

// NewManager creates an empty manager.
func NewManager(cfg *config.Config, reg *mcp.Registry, exec *tools.Executor) *Manager {
	return &Manager{cfg: cfg, MCP: reg, Exec: exec, provs: map[string]provider.Provider{}}
}

// ProviderFor returns (and caches) the provider instance for a config entry.
func (m *Manager) ProviderFor(name string) (provider.Provider, error) {
	if p, ok := m.provs[name]; ok {
		return p, nil
	}
	pcfg, ok := m.cfg.Providers[name]
	if !ok {
		return nil, fmt.Errorf("no provider %q in config", name)
	}
	p, err := provider.New(name, pcfg)
	if err != nil {
		return nil, err
	}
	m.provs[name] = p
	return p, nil
}

// ReloadProviders drops the provider cache (after a config edit) and
// re-resolves every agent's provider from the new config.
func (m *Manager) ReloadProviders() {
	m.provs = map[string]provider.Provider{}
	for _, a := range m.Agents {
		if p, err := m.ProviderFor(a.ProviderName); err != nil {
			a.Provider = nil
			a.ProviderErr = err.Error()
		} else {
			a.Provider = p
			a.ProviderErr = ""
		}
	}
}

// NewAgent creates an agent from config defaults. A provider construction
// failure is recorded on the agent rather than returned, so the UI can show
// it and let the user switch providers.
func (m *Manager) NewAgent() *Agent {
	m.nextID++
	a := &Agent{
		ID:             m.nextID,
		Name:           fmt.Sprintf("agent-%d", m.nextID),
		ProviderName:   m.cfg.DefaultProvider,
		Model:          m.cfg.DefaultModel,
		SystemPrompt:   m.cfg.SystemPrompt,
		MaxTokens:      m.cfg.MaxTokens,
		Temperature:    m.cfg.Temperature,
		Thinking:       m.cfg.Thinking,
		ThinkingBudget: m.cfg.ThinkingBudget,
		ToolsEnabled:   true,
		Events:         make(chan Event, 512),
		MCP:            m.MCP,
		Exec:           m.Exec,
	}
	if p, err := m.ProviderFor(a.ProviderName); err != nil {
		a.ProviderErr = err.Error()
	} else {
		a.Provider = p
	}
	m.Agents = append(m.Agents, a)
	return a
}

// Fork clones src (settings, system prompt, goal and full history) into a new
// agent, so the conversation can branch.
func (m *Manager) Fork(src *Agent) *Agent {
	m.nextID++
	a := &Agent{
		ID:             m.nextID,
		Name:           fmt.Sprintf("agent-%d", m.nextID),
		ProviderName:   src.ProviderName,
		Provider:       src.Provider,
		ProviderErr:    src.ProviderErr,
		Model:          src.Model,
		SystemPrompt:   src.SystemPrompt,
		Goal:           src.Goal,
		MaxTokens:      src.MaxTokens,
		Temperature:    src.Temperature,
		Thinking:       src.Thinking,
		ThinkingBudget: src.ThinkingBudget,
		ToolsEnabled:   src.ToolsEnabled,
		CtxTokens:      src.CtxTokens,
		Events:         make(chan Event, 512),
		MCP:            m.MCP,
		Exec:           m.Exec,
	}
	a.Messages = make([]provider.Message, len(src.Messages))
	copy(a.Messages, src.Messages)
	m.Agents = append(m.Agents, a)
	return a
}

// ActiveAgent returns the currently selected agent, or nil.
func (m *Manager) ActiveAgent() *Agent {
	if len(m.Agents) == 0 {
		return nil
	}
	if m.Active < 0 || m.Active >= len(m.Agents) {
		m.Active = 0
	}
	return m.Agents[m.Active]
}

// ByID finds an agent by id.
func (m *Manager) ByID(id int) *Agent {
	for _, a := range m.Agents {
		if a.ID == id {
			return a
		}
	}
	return nil
}

// Next selects the next agent.
func (m *Manager) Next() {
	if len(m.Agents) > 0 {
		m.Active = (m.Active + 1) % len(m.Agents)
	}
}

// Prev selects the previous agent.
func (m *Manager) Prev() {
	if len(m.Agents) > 0 {
		m.Active = (m.Active - 1 + len(m.Agents)) % len(m.Agents)
	}
}

// Close removes the agent at index i, cancelling any in-flight turn. The last
// agent cannot be removed.
func (m *Manager) Close(i int) {
	if len(m.Agents) <= 1 || i < 0 || i >= len(m.Agents) {
		return
	}
	a := m.Agents[i]
	a.Cancel()
	// Drain remaining events so the run goroutine can exit.
	go func() {
		for {
			select {
			case <-a.Events:
			case <-time.After(10 * time.Second):
				return
			}
		}
	}()
	m.Agents = append(m.Agents[:i], m.Agents[i+1:]...)
	if m.Active >= len(m.Agents) {
		m.Active = len(m.Agents) - 1
	}
}

// ToSession snapshots the workspace.
func (m *Manager) ToSession(id, name string, createdAt time.Time) *session.Session {
	s := &session.Session{ID: id, Name: name, CreatedAt: createdAt, Active: m.Active}
	for _, a := range m.Agents {
		s.Agents = append(s.Agents, session.AgentState{
			Name:           a.Name,
			Provider:       a.ProviderName,
			Model:          a.Model,
			SystemPrompt:   a.SystemPrompt,
			Goal:           a.Goal,
			MaxTokens:      a.MaxTokens,
			Temperature:    a.Temperature,
			Thinking:       a.Thinking,
			ThinkingBudget: a.ThinkingBudget,
			ToolsEnabled:   a.ToolsEnabled,
			CtxTokens:      a.CtxTokens,
			Messages:       a.Messages,
			Stats:          a.Stats,
		})
	}
	return s
}

// LoadSession replaces the workspace with a saved session's agents.
func (m *Manager) LoadSession(s *session.Session) {
	for i := range m.Agents {
		m.Agents[i].Cancel()
	}
	m.Agents = nil
	for _, st := range s.Agents {
		m.nextID++
		a := &Agent{
			ID:             m.nextID,
			Name:           st.Name,
			ProviderName:   st.Provider,
			Model:          st.Model,
			SystemPrompt:   st.SystemPrompt,
			Goal:           st.Goal,
			MaxTokens:      st.MaxTokens,
			Temperature:    st.Temperature,
			Thinking:       st.Thinking,
			ThinkingBudget: st.ThinkingBudget,
			ToolsEnabled:   st.ToolsEnabled,
			CtxTokens:      st.CtxTokens,
			Messages:       st.Messages,
			Stats:          st.Stats,
			Events:         make(chan Event, 512),
			MCP:            m.MCP,
			Exec:           m.Exec,
		}
		if a.MaxTokens <= 0 {
			a.MaxTokens = m.cfg.MaxTokens
		}
		if p, err := m.ProviderFor(a.ProviderName); err != nil {
			a.ProviderErr = err.Error()
		} else {
			a.Provider = p
		}
		m.Agents = append(m.Agents, a)
	}
	m.Active = s.Active
	if len(m.Agents) == 0 {
		m.NewAgent()
		m.Active = 0
	}
	if m.Active >= len(m.Agents) {
		m.Active = 0
	}
}
