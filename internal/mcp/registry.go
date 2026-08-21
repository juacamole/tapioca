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

// Add registers a connected client, unless another server already answers to
// the same namespaced name.
func (r *Registry) Add(c *Client) {
	r.mu.Lock()
	other, taken := r.collision(c)
	if taken {
		r.errors[c.Name] = collisionMsg(c.Name, other)
	} else {
		r.clients = append(r.clients, c)
	}
	r.mu.Unlock()
	if taken {
		// Nothing can route to it and nothing may grant to it, so leaving the
		// process running would only be a subprocess with no way to be reached.
		c.Close()
	}
}

// collision reports a connected server that c cannot be told apart from once
// its name is namespaced. The caller must hold r.mu.
//
// sanitize maps every character it does not accept onto '-', which makes it
// many-to-one: "trusted mcp", "trusted.mcp" and "trusted/mcp" all come out as
// "trusted-mcp". The namespaced name is the whole of what a permission prompt
// shows, the whole of a session grant's key — tools.go states the invariant
// that "a grant for one server's delete must not cover another's" — and the
// whole of what Call routes on. So a second server named a hyphen apart from a
// trusted one inherits its grants and takes its calls, decided by nothing more
// than which [[mcp]] block comes first. An HTTP endpoint is a low-alarm thing
// to be told to add to a config, and this made adding one enough.
//
// The server already connected keeps the name; the newcomer is refused and
// says why, rather than both being dropped and a working setup breaking on the
// day something else in the list changed its name.
func (r *Registry) collision(c *Client) (string, bool) {
	for _, existing := range r.clients {
		if existing.Name != c.Name && sanitize(existing.Name) == sanitize(c.Name) {
			return existing.Name, true
		}
	}
	return "", false
}

func collisionMsg(name, other string) string {
	return fmt.Sprintf("name collides with %q: both become %q once namespaced, so their tools, "+
		"their permission prompts and their session grants would be one and the same", other, sanitize(name))
}

// SetError records a failed connection for display.
func (r *Registry) SetError(name, msg string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.errors[name] = msg
}

// Replace swaps in a reconnected client, dropping any earlier one for the same
// name along with the error that was recorded against it. A server connected a
// second time — after a login, say — would otherwise be listed twice, with its
// tools offered twice under one name, and go on showing the failure that the
// reconnection has just fixed.
func (r *Registry) Replace(c *Client) {
	r.mu.Lock()
	kept := r.clients[:0]
	var old []*Client
	for _, existing := range r.clients {
		if existing.Name == c.Name {
			old = append(old, existing)
			continue
		}
		kept = append(kept, existing)
	}
	r.clients = kept
	// The same guard Add applies: reconnecting is another way in, and a name
	// that could not be added must not become addable by logging in.
	if other, taken := r.collision(c); taken {
		r.errors[c.Name] = collisionMsg(c.Name, other)
		old = append(old, c)
	} else {
		r.clients = append(r.clients, c)
		delete(r.errors, c.Name)
	}
	r.mu.Unlock()
	// Outside the lock: closing an HTTP client sends a request to end its
	// session, and holding the registry for the length of that stalls every
	// tool call in the app.
	for _, o := range old {
		o.Close()
	}
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

// route resolves a namespaced tool name back to the server it was built from.
//
// Not by cutting at the first "__": FullName joins with it, but a server's own
// name may contain one too, and a cut splits where the name *can* be split
// rather than where it *was* joined. "gh__issues__list" then reads as server
// "gh" no matter which server listed it, so every tool on a server named
// "gh__issues" was dispatched to another server — and the permission prompt
// named the server that never ran it. Matching whole server names, and
// preferring the one that actually lists the tool, puts the call where the
// name says it goes.
//
// Two servers can still both claim one name ("gh" with a tool "issues__list",
// "gh__issues" with a tool "list"). There is nothing to prefer between them
// and silently picking one is the failure this exists to stop, so it is
// refused and both are named.
func (r *Registry) route(fullName string) (*Client, string, error) {
	var owners []*Client
	tools := map[*Client]string{}
	var longest *Client
	for _, c := range r.Clients() {
		prefix := sanitize(c.Name) + "__"
		if !strings.HasPrefix(fullName, prefix) {
			continue
		}
		tool := fullName[len(prefix):]
		tools[c] = tool
		if longest == nil || len(c.Name) > len(longest.Name) {
			longest = c
		}
		if c.hasTool(tool) {
			owners = append(owners, c)
		}
	}
	switch {
	case len(owners) == 1:
		return owners[0], tools[owners[0]], nil
	case len(owners) > 1:
		names := make([]string, 0, len(owners))
		for _, c := range owners {
			names = append(names, c.Name)
		}
		sort.Strings(names)
		return nil, "", fmt.Errorf("tool %q is offered by more than one server (%s); rename one of them",
			fullName, strings.Join(names, ", "))
	case longest != nil:
		// No server lists the tool — a list that changed since it was offered,
		// most likely. The server named by the longest matching prefix is the
		// one the name was built from; let it report the unknown tool itself.
		return longest, tools[longest], nil
	}
	return nil, "", fmt.Errorf("no mcp server for tool %q", fullName)
}

// Call routes a namespaced tool call to the owning server.
func (r *Registry) Call(ctx context.Context, fullName string, args json.RawMessage) (string, bool, error) {
	c, tool, err := r.route(fullName)
	if err != nil {
		return "", true, err
	}
	if !c.Alive() {
		return "", true, fmt.Errorf("mcp server %s is not running", c.Name)
	}
	return c.CallTool(ctx, tool, args)
}

// CloseAll terminates every server.
func (r *Registry) CloseAll() {
	for _, c := range r.Clients() {
		c.Close()
	}
}
