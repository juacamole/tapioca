package lsp

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"time"

	"tapioca/internal/config"
)

const (
	// diagWait bounds how long an edit waits for a verdict. A cold server is
	// still loading the workspace; making the agent wait longer than this
	// costs more than the diagnostics are worth on that first edit.
	diagWait = 5 * time.Second
	maxShown = 20
)

// Registry starts language servers on demand and reports on edited files.
// Servers are shared for the whole session, so only the first edit pays the
// startup cost.
type Registry struct {
	mu      sync.Mutex
	cfgs    []config.LSPServerConfig
	root    string
	clients map[string]*Client // by server name
	failed  map[string]string  // name -> why, so we complain only once
}

func NewRegistry(cfgs []config.LSPServerConfig, root string) *Registry {
	return &Registry{
		cfgs: cfgs, root: root,
		clients: map[string]*Client{},
		failed:  map[string]string{},
	}
}

// Warm starts every configured server up front. Language servers spend their
// first seconds loading the workspace and answer nothing useful until they
// have; starting on the first edit means that edit is the one that gets no
// diagnostics — exactly the edit the agent most wants checked.
func (r *Registry) Warm() {
	r.mu.Lock()
	cfgs := append([]config.LSPServerConfig(nil), r.cfgs...)
	r.mu.Unlock()
	for _, cfg := range cfgs {
		r.start(cfg)
	}
}

// start brings up one server, remembering a failure so it is not retried on
// every edit.
func (r *Registry) start(cfg config.LSPServerConfig) *Client {
	r.mu.Lock()
	if c, ok := r.clients[cfg.Name]; ok {
		r.mu.Unlock()
		return c
	}
	if _, bad := r.failed[cfg.Name]; bad {
		r.mu.Unlock()
		return nil
	}
	r.mu.Unlock()

	// Starting outside the lock: gopls takes a moment, and holding it here
	// would stall every edit behind the handshake.
	var c *Client
	var err error
	if _, lookErr := exec.LookPath(cfg.Command); lookErr != nil {
		err = fmt.Errorf("%s not found", cfg.Command)
	} else {
		c, err = Start(context.Background(), cfg, r.root)
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if err != nil {
		r.failed[cfg.Name] = err.Error()
		return nil
	}
	if existing, ok := r.clients[cfg.Name]; ok {
		c.Close() // another edit raced us here
		return existing
	}
	r.clients[cfg.Name] = c
	return c
}

// clientFor returns a started server covering path, or nil.
func (r *Registry) clientFor(path string) *Client {
	r.mu.Lock()
	cfgs := append([]config.LSPServerConfig(nil), r.cfgs...)
	r.mu.Unlock()
	for _, cfg := range cfgs {
		if handles(cfg.Extensions, path) {
			return r.start(cfg)
		}
	}
	return nil
}

// Check reports the problems in a file after an edit, formatted for the model.
// An empty string means nothing to say — no server, a clean file, or a server
// that has not finished loading the workspace yet.
func (r *Registry) Check(path string) string {
	c := r.clientFor(path)
	if c == nil {
		return ""
	}
	text, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	if err := c.Sync(path, string(text)); err != nil {
		return ""
	}
	diags := c.Diagnostics(path, diagWait)
	if allNotReady(diags) {
		// Saying nothing beats reporting "no active builds contain this file",
		// which is about the server's startup and not about the edit — the
		// model reads it as a real problem and goes looking for one.
		return ""
	}
	return Format(diags)
}

// notReadyPhrases are what servers publish while they are still loading a
// workspace, rather than a verdict on the code.
var notReadyPhrases = []string{
	"no active builds",
	"not contained in",
	"no package for",
	"is not in a module",
	"loading packages",
	"still loading",
}

func allNotReady(diags []Diagnostic) bool {
	if len(diags) == 0 {
		return false
	}
	for _, d := range diags {
		msg := strings.ToLower(d.Message)
		hit := false
		for _, p := range notReadyPhrases {
			if strings.Contains(msg, p) {
				hit = true
				break
			}
		}
		if !hit {
			return false
		}
	}
	return true
}

// Format renders diagnostics as the note appended to a tool result. Errors
// come first: they are what stops the code working.
func Format(diags []Diagnostic) string {
	if len(diags) == 0 {
		return ""
	}
	sort.SliceStable(diags, func(i, j int) bool {
		if diags[i].Severity != diags[j].Severity {
			return diags[i].Severity < diags[j].Severity
		}
		return diags[i].Line < diags[j].Line
	})
	errs := 0
	for _, d := range diags {
		if d.Severity == SeverityError {
			errs++
		}
	}
	shown := diags
	if len(shown) > maxShown {
		shown = shown[:maxShown]
	}
	var b strings.Builder
	fmt.Fprintf(&b, "diagnostics: %d problem(s)", len(diags))
	if errs > 0 {
		fmt.Fprintf(&b, ", %d error(s)", errs)
	}
	for _, d := range shown {
		fmt.Fprintf(&b, "\n  %s", d)
	}
	if len(diags) > len(shown) {
		fmt.Fprintf(&b, "\n  … %d more", len(diags)-len(shown))
	}
	return b.String()
}

// Errors reports the servers that could not be started, for /permissions-style
// visibility rather than silent absence.
func (r *Registry) Errors() map[string]string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := map[string]string{}
	for k, v := range r.failed {
		out[k] = v
	}
	return out
}

// Running lists the servers currently up.
func (r *Registry) Running() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var names []string
	for n := range r.clients {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

func (r *Registry) CloseAll() {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, c := range r.clients {
		c.Close()
	}
	r.clients = map[string]*Client{}
}

func handles(exts []string, path string) bool {
	c := &Client{Exts: exts}
	return c.Handles(path)
}
