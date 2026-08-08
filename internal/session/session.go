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
	"sort"
	"strings"
	"time"

	"tapioca/internal/config"
	"tapioca/internal/provider"
	"tapioca/internal/stats"
)

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
	Stats          stats.Stats        `json:"stats"`
}

// Session is a full saved workspace.
type Session struct {
	ID        string       `json:"id"`
	Name      string       `json:"name"`
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
	UpdatedAt time.Time
	Agents    int
	Messages  int
	Blob      string
}

// Dir returns the session storage directory.
func Dir() string { return filepath.Join(config.DataDir(), "sessions") }

// NewID returns a timestamp-based session id with a random suffix, so two
// same-second launches can never claim the same file.
func NewID() string {
	var b [2]byte
	_, _ = rand.Read(b[:])
	return time.Now().Format("20060102-150405") + "-" + hex.EncodeToString(b[:])
}

func pathFor(id string) string { return filepath.Join(Dir(), id+".json") }

// Save writes the session atomically.
func (s *Session) Save() error {
	if err := os.MkdirAll(Dir(), 0o755); err != nil {
		return err
	}
	s.UpdatedAt = time.Now()
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	tmp := pathFor(s.ID) + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
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

// List returns stored sessions, newest first.
func List() ([]Meta, error) {
	entries, err := os.ReadDir(Dir())
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var metas []Meta
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		id := strings.TrimSuffix(e.Name(), ".json")
		s, err := Load(id)
		if err != nil {
			continue
		}
		msgs := 0
		var blob strings.Builder
		for _, a := range s.Agents {
			msgs += len(a.Messages)
			for _, m := range a.Messages {
				if blob.Len() > 20_000 {
					break
				}
				if !m.IsToolResult() {
					blob.WriteString(strings.ToLower(m.Text()))
					blob.WriteString("\n")
				}
			}
		}
		metas = append(metas, Meta{ID: s.ID, Name: s.Name, UpdatedAt: s.UpdatedAt,
			Agents: len(s.Agents), Messages: msgs, Blob: blob.String()})
	}
	sort.Slice(metas, func(i, j int) bool { return metas[i].UpdatedAt.After(metas[j].UpdatedAt) })
	return metas, nil
}

// LatestID returns the most recently updated session id.
func LatestID() (string, error) {
	metas, err := List()
	if err != nil {
		return "", err
	}
	if len(metas) == 0 {
		return "", fmt.Errorf("no saved sessions")
	}
	return metas[0].ID, nil
}
