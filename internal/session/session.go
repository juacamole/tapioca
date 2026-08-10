// Package session persists whole workspaces (all agents, their histories and
// stats) as JSON files so they can be resumed later.
package session

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"tapioca/internal/config"
	"tapioca/internal/provider"
	"tapioca/internal/stats"
)

// TodoItem is one entry of an agent's plan. It lives here because agents
// import session, not the other way round; agent aliases it.
type TodoItem struct {
	Content string `json:"content"`
	Status  string `json:"status"`
}

// AgentState is one agent's serializable state.
type AgentState struct {
	Name           string             `json:"name"`
	Provider       string             `json:"provider"`
	Model          string             `json:"model"`
	SystemPrompt   string             `json:"system_prompt"`
	Goal           string             `json:"goal,omitempty"`
	MaxTokens      int                `json:"max_tokens"`
	Temperature    float64            `json:"temperature"`
	Thinking       bool               `json:"thinking"`
	ThinkingBudget int                `json:"thinking_budget"`
	ToolsEnabled   bool               `json:"tools_enabled"`
	CtxTokens      int                `json:"ctx_tokens,omitempty"`
	Messages       []provider.Message `json:"messages"`
	Queue          []provider.Message `json:"queue,omitempty"`
	Todos          []TodoItem         `json:"todos,omitempty"`
	Depth          int                `json:"depth,omitempty"`
	Stats          stats.Stats        `json:"stats"`
}

// Session is a full saved workspace.
type Session struct {
	ID        string       `json:"id"`
	Name      string       `json:"name"`
	Cwd       string       `json:"cwd,omitempty"` // project the session belongs to
	CreatedAt time.Time    `json:"created_at"`
	UpdatedAt time.Time    `json:"updated_at"`
	Active    int          `json:"active"`
	Agents    []AgentState `json:"agents"`
}

// Meta summarizes a stored session for pickers. Blob holds lowercased
// message text so the session picker can search across conversations.
type Meta struct {
	ID        string
	Name      string
	Cwd       string
	UpdatedAt time.Time
	Agents    int
	Messages  int
	Blob      string
}

// searchBlobMax caps the per-session text kept for the /resume filter.
const searchBlobMax = 20_000

// Dir returns the session storage directory.
func Dir() string { return filepath.Join(config.DataDir(), "sessions") }

var (
	idMu     sync.Mutex
	idIssued = map[string]bool{}
)

// NewID returns a timestamp-based session id with a random suffix. A repeat
// would overwrite the older session, so ids already issued in this process or
// already on disk are rejected rather than merely made unlikely.
func NewID() string {
	idMu.Lock()
	defer idMu.Unlock()
	for {
		var b [3]byte
		_, _ = rand.Read(b[:])
		id := time.Now().Format("20060102-150405") + "-" + hex.EncodeToString(b[:])
		if idIssued[id] {
			continue
		}
		if _, err := os.Stat(pathFor(id)); err == nil {
			continue
		}
		idIssued[id] = true
		return id
	}
}

func pathFor(id string) string { return filepath.Join(Dir(), id+".json") }

// Save writes the session atomically.
func (s *Session) Save() error {
	if err := os.MkdirAll(Dir(), 0o700); err != nil {
		return err
	}
	s.UpdatedAt = time.Now()
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	tmp := pathFor(s.ID) + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, pathFor(s.ID))
}

// Load reads a session by id.
func Load(id string) (*Session, error) {
	data, err := os.ReadFile(pathFor(id))
	if err != nil {
		return nil, fmt.Errorf("loading session %s: %w", id, err)
	}
	var s Session
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, fmt.Errorf("parsing session %s: %w", id, err)
	}
	// The filename is the identity: a copied file must save under the name
	// it was loaded from, not clobber the original via its embedded id.
	s.ID = id
	return &s, nil
}

// ForProject splits sessions into those belonging to cwd and the rest. Older
// sessions predate the recorded directory and count as "elsewhere" rather than
// being claimed by whichever project you happen to be in.
func ForProject(metas []Meta, cwd string) (here, elsewhere []Meta) {
	for _, m := range metas {
		if m.Cwd != "" && sameDir(m.Cwd, cwd) {
			here = append(here, m)
		} else {
			elsewhere = append(elsewhere, m)
		}
	}
	return here, elsewhere
}

func sameDir(a, b string) bool {
	return filepath.Clean(a) == filepath.Clean(b)
}

// LatestID returns the most recently updated session for cwd, falling back to
// the newest overall so `-c` still works before any session records a
// directory. Pass "" to ignore the project entirely.
func LatestID(cwd string) (string, error) {
	metas, err := List()
	if err != nil {
		return "", err
	}
	if len(metas) == 0 {
		return "", fmt.Errorf("no saved sessions")
	}
	if cwd != "" {
		if here, _ := ForProject(metas, cwd); len(here) > 0 {
			return here[0].ID, nil
		}
	}
	return metas[0].ID, nil
}
