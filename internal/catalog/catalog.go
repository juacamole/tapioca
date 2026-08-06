// Package catalog provides model metadata (context windows, prices) from a
// cached models.dev snapshot, with runtime overrides for locally probed
// models.
package catalog

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"tapioca/internal/config"
)

const (
	sourceURL = "https://models.dev/api.json"
	ttl       = 24 * time.Hour
	maxBody   = 16 << 20
)

// Model is the metadata Tapioca cares about. Costs are $/Mtok.
type Model struct {
	Context int
	Output  int
	In      float64
	Out     float64
	CacheR  float64
	CacheW  float64
}

var (
	mu   sync.RWMutex
	byID = map[string]Model{}
)

func file() string { return filepath.Join(config.DataDir(), "models.json") }

// Load populates the catalog from the on-disk snapshot, if any.
func Load() {
	data, err := os.ReadFile(file())
	if err != nil {
		return
	}
	if m := parse(data); len(m) > 0 {
		mu.Lock()
		byID = m
		mu.Unlock()
	}
}

// Refresh fetches a new snapshot when the cached one is stale. Safe to run
// in a goroutine; failures keep the old data.
func Refresh() {
	if st, err := os.Stat(file()); err == nil && time.Since(st.ModTime()) < ttl {
		return
	}
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Get(sourceURL)
	if err != nil {
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return
	}
	m := parse(data)
	if len(m) == 0 {
		return
	}
	if err := os.MkdirAll(filepath.Dir(file()), 0o755); err != nil {
		return
	}
	tmp := file() + ".tmp"
	if os.WriteFile(tmp, data, 0o644) == nil {
		_ = os.Rename(tmp, file())
	}
	mu.Lock()
	for k, v := range m {
		byID[k] = v
	}
	mu.Unlock()
}

type sourceModel struct {
	Limit struct {
		Context int `json:"context"`
		Output  int `json:"output"`
	} `json:"limit"`
	Cost struct {
		Input      float64 `json:"input"`
		Output     float64 `json:"output"`
		CacheRead  float64 `json:"cache_read"`
		CacheWrite float64 `json:"cache_write"`
	} `json:"cost"`
}

func parse(data []byte) map[string]Model {
	var raw map[string]struct {
		Models map[string]sourceModel `json:"models"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil
	}
	// First-party providers win id collisions with aggregators.
	preferred := []string{"anthropic", "openai", "google", "mistral", "deepseek", "xai"}
	order := make([]string, 0, len(raw))
	rest := make([]string, 0, len(raw))
	seenProv := map[string]bool{}
	for _, p := range preferred {
		if _, ok := raw[p]; ok {
			order = append(order, p)
			seenProv[p] = true
		}
	}
	for p := range raw {
		if !seenProv[p] {
			rest = append(rest, p)
		}
	}
	sort.Strings(rest)
	order = append(order, rest...)

	out := map[string]Model{}
	for _, prov := range order {
		for id, sm := range raw[prov].Models {
			key := normalize(id)
			if _, exists := out[key]; exists {
				continue
			}
			out[key] = Model{
				Context: sm.Limit.Context,
				Output:  sm.Limit.Output,
				In:      sm.Cost.Input,
				Out:     sm.Cost.Output,
				CacheR:  sm.Cost.CacheRead,
				CacheW:  sm.Cost.CacheWrite,
			}
		}
	}
	return out
}

func normalize(id string) string {
	id = strings.ToLower(id)
	if i := strings.IndexByte(id, ':'); i > 0 {
		id = id[:i]
	}
	return id
}

// Lookup finds metadata for a model id (tag suffixes ignored).
func Lookup(id string) (Model, bool) {
	mu.RLock()
	defer mu.RUnlock()
	m, ok := byID[normalize(id)]
	return m, ok
}

// Store records a runtime override, e.g. a probed local model.
func Store(id string, m Model) {
	mu.Lock()
	byID[normalize(id)] = m
	mu.Unlock()
}
