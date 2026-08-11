package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"

	"tapioca/internal/provider"
)

// Registry holds every connected MCP client and routes tool calls to them.
// Tool names are namespaced as "<server>__<tool>" to avoid collisions.
type Registry struct {
	mu      sync.Mutex
	clients []*Client
	errors  map[string]string // server name -> connection error
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry {
	return &Registry{errors: map[string]string{}}
}

// Add registers a connected client.
func (r *Registry) Add(c *Client) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.clients = append(r.clients, c)
}

// SetError records a failed connection for display.
func (r *Registry) SetError(name, msg string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.errors[name] = msg
}

// Clients returns a snapshot of connected clients.
func (r *Registry) Clients() []*Client {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]*Client, len(r.clients))
	copy(out, r.clients)
	return out
}

// Errors returns a snapshot of connection errors by server name.
func (r *Registry) Errors() map[string]string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make(map[string]string, len(r.errors))
	for k, v := range r.errors {
		out[k] = v
	}
	return out
}

// Sanitize exposes the namespacing rule so the UI can build the same names.
func Sanitize(s string) string { return sanitize(s) }

func sanitize(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	return b.String()
}

// FullName returns the namespaced tool name for a server tool.
func FullName(server, tool string) string {
	return sanitize(server) + "__" + tool
}

// AllTools returns every available tool as provider tool definitions.
func (r *Registry) AllTools() []provider.ToolDef {
	var defs []provider.ToolDef
	for _, c := range r.Clients() {
		if !c.Alive() {
			continue
		}
		for _, t := range c.Tools() {
			defs = append(defs, provider.ToolDef{
				Name:        FullName(c.Name, t.Name),
				Description: fmt.Sprintf("[%s] %s", c.Name, t.Description),
				InputSchema: t.InputSchema,
			})
		}
	}
	return defs
}

// AllPrompts returns every prompt on every live server.
func (r *Registry) AllPrompts() []Prompt {
	var out []Prompt
	for _, c := range r.Clients() {
		if c.Alive() {
			out = append(out, c.Prompts()...)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Server != out[j].Server {
			return out[i].Server < out[j].Server
		}
		return out[i].Name < out[j].Name
	})
	return out
}

// AllResources returns every resource on every live server.
func (r *Registry) AllResources() []Resource {
	var out []Resource
	for _, c := range r.Clients() {
		if c.Alive() {
			out = append(out, c.Resources()...)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Server != out[j].Server {
			return out[i].Server < out[j].Server
		}
		return out[i].URI < out[j].URI
	})
	return out
}

// client finds a live client by its sanitized name.
func (r *Registry) client(server string) (*Client, error) {
	for _, c := range r.Clients() {
		if sanitize(c.Name) != server {
			continue
		}
		if !c.Alive() {
			return nil, fmt.Errorf("mcp server %s is not running", c.Name)
		}
		return c, nil
	}
	return nil, fmt.Errorf("no mcp server named %q", server)
}

// GetPrompt renders a prompt on the named server.
func (r *Registry) GetPrompt(ctx context.Context, server, name string, args map[string]string) (string, error) {
	c, err := r.client(server)
	if err != nil {
		return "", err
	}
	return c.GetPrompt(ctx, name, args)
}

// ReadResource reads a resource from the named server.
func (r *Registry) ReadResource(ctx context.Context, server, uri string) (string, error) {
	c, err := r.client(server)
	if err != nil {
		return "", err
	}
	return c.ReadResource(ctx, uri)
}

// Call routes a namespaced tool call to the owning server.
func (r *Registry) Call(ctx context.Context, fullName string, args json.RawMessage) (string, bool, error) {
	server, tool, ok := strings.Cut(fullName, "__")
	if !ok {
		return "", true, fmt.Errorf("unknown tool %q", fullName)
	}
	for _, c := range r.Clients() {
		if sanitize(c.Name) != server {
			continue
		}
		if !c.Alive() {
			return "", true, fmt.Errorf("mcp server %s is not running", c.Name)
		}
		return c.CallTool(ctx, tool, args)
	}
	return "", true, fmt.Errorf("no mcp server for tool %q", fullName)
}

// CloseAll terminates every server.
func (r *Registry) CloseAll() {
	for _, c := range r.Clients() {
		c.Close()
	}
}
