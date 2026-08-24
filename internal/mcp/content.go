package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// Servers offer more than tools. Prompts are templates a person invokes on
// purpose, and resources are documents to put in front of the model — neither
// is something the model calls on its own, so they arrive through the same
// doors as their local equivalents: prompts become slash commands, resources
// are attached with @.

// Prompt is one prompt template a server offers.
type Prompt struct {
	Server      string
	Name        string
	Description string
	Arguments   []PromptArg
}

// PromptArg is one argument a prompt takes.
type PromptArg struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Required    bool   `json:"required"`
}

// Resource is one document a server offers.
type Resource struct {
	Server      string
	URI         string
	Name        string
	Description string
	MimeType    string
}

// serverCaps is what the server said it can do at initialize. Listing prompts
// on a server that has none is an error reply, not an empty list, so the
// capabilities decide what is even asked for.
type serverCaps struct {
	Tools     *struct{} `json:"tools"`
	Prompts   *struct{} `json:"prompts"`
	Resources *struct{} `json:"resources"`
}

func (c *Client) listPrompts(ctx context.Context) error {
	res, err := c.call(ctx, "prompts/list", map[string]any{})
	if err != nil {
		return fmt.Errorf("prompts/list: %w", err)
	}
	var list struct {
		Prompts []struct {
			Name        string      `json:"name"`
			Description string      `json:"description"`
			Arguments   []PromptArg `json:"arguments"`
		} `json:"prompts"`
	}
	if err := json.Unmarshal(res, &list); err != nil {
		return fmt.Errorf("decoding prompts: %w", err)
	}
	prompts := make([]Prompt, 0, len(list.Prompts))
	for _, p := range list.Prompts {
		prompts = append(prompts, Prompt{
			Server: c.Name, Name: p.Name, Description: p.Description, Arguments: p.Arguments,
		})
	}
	c.toolsMu.Lock()
	c.prompts = prompts
	c.toolsMu.Unlock()
	return nil
}

func (c *Client) listResources(ctx context.Context) error {
	res, err := c.call(ctx, "resources/list", map[string]any{})
	if err != nil {
		return fmt.Errorf("resources/list: %w", err)
	}
	var list struct {
		Resources []struct {
			URI         string `json:"uri"`
			Name        string `json:"name"`
			Description string `json:"description"`
			MimeType    string `json:"mimeType"`
		} `json:"resources"`
	}
	if err := json.Unmarshal(res, &list); err != nil {
		return fmt.Errorf("decoding resources: %w", err)
	}
	resources := make([]Resource, 0, len(list.Resources))
	for _, r := range list.Resources {
		resources = append(resources, Resource{
			Server: c.Name, URI: r.URI, Name: r.Name, Description: r.Description, MimeType: r.MimeType,
		})
	}
	c.toolsMu.Lock()
	c.resources = resources
	c.toolsMu.Unlock()
	return nil
}

// Prompts returns the server's prompt templates.
func (c *Client) Prompts() []Prompt {
	c.toolsMu.Lock()
	defer c.toolsMu.Unlock()
	return append([]Prompt(nil), c.prompts...)
}

// Resources returns the server's resources.
func (c *Client) Resources() []Resource {
	c.toolsMu.Lock()
	defer c.toolsMu.Unlock()
	return append([]Resource(nil), c.resources...)
}

// GetPrompt renders a prompt template into the text to send.
func (c *Client) GetPrompt(ctx context.Context, name string, args map[string]string) (string, error) {
	params := map[string]any{"name": name}
	if len(args) > 0 {
		params["arguments"] = args
	}
	res, err := c.call(ctx, "prompts/get", params)
	if err != nil {
		return "", err
	}
	var out struct {
		Description string `json:"description"`
		Messages    []struct {
			Role    string `json:"role"`
			Content struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(res, &out); err != nil {
		return "", fmt.Errorf("decoding prompt: %w", err)
	}
	var parts []string
	for _, m := range out.Messages {
		switch {
		case m.Content.Type == "text" && m.Content.Text != "":
			parts = append(parts, m.Content.Text)
		case m.Content.Type != "":
			parts = append(parts, fmt.Sprintf("[%s content omitted]", m.Content.Type))
		}
	}
	if len(parts) == 0 {
		return "", fmt.Errorf("prompt %s returned nothing", name)
	}
	return strings.Join(parts, "\n\n"), nil
}

// maxResourceBytes caps what one resource may add to a prompt. Resources can
// be whole databases behind a URI, and blowing the context window in one
// mention is worse than a truncated read.
const maxResourceBytes = 256 * 1024

// ReadResource fetches a resource's text. Binary contents are reported rather
// than base64-dumped into the conversation.
func (c *Client) ReadResource(ctx context.Context, uri string) (string, error) {
	res, err := c.call(ctx, "resources/read", map[string]any{"uri": uri})
	if err != nil {
		return "", err
	}
	var out struct {
		Contents []struct {
			URI      string `json:"uri"`
			MimeType string `json:"mimeType"`
			Text     string `json:"text"`
			Blob     string `json:"blob"`
		} `json:"contents"`
	}
	if err := json.Unmarshal(res, &out); err != nil {
		return "", fmt.Errorf("decoding resource: %w", err)
	}
	var b strings.Builder
	for _, item := range out.Contents {
		part := item.Text
		if part == "" {
			if item.Blob == "" {
				continue
			}
			// The placeholder answers to the budget the text does. It carries a
			// URI the server chose and the check sat after the `continue`, so a
			// response made of nothing but tiny blob entries put every one of
			// those URIs into the conversation with no cap at all — the one
			// branch of two that the limit did not reach.
			part = fmt.Sprintf("[%s: binary content omitted]", item.URI)
		}
		if b.Len()+len(part) > maxResourceBytes {
			b.WriteString(part[:max(0, maxResourceBytes-b.Len())])
			b.WriteString("\n[truncated]")
			break
		}
		b.WriteString(part)
		b.WriteString("\n")
	}
	text := strings.TrimRight(b.String(), "\n")
	if text == "" {
		return "", fmt.Errorf("resource %s returned no text", uri)
	}
	return text, nil
}

// refreshPrompts and refreshResources handle the list_changed notifications
// for their kind, the same way refreshTools does.
func (c *Client) refreshPrompts() { c.refreshList("prompts", c.listPrompts) }

func (c *Client) refreshResources() { c.refreshList("resources", c.listResources) }

// refreshList re-lists in the background, coalescing: a server that floods
// list_changed notifications spawned one goroutine and one pending request per
// notification, which reached 50,000 of each in four seconds. At most one
// refresh per kind runs at a time, and one more is queued behind it so a
// change arriving mid-refresh is not missed.
func (c *Client) refreshList(kind string, list func(context.Context) error) {
	c.toolsMu.Lock()
	if c.refreshing == nil {
		c.refreshing = map[string]bool{}
	}
	if c.refreshing[kind] {
		c.pendingRefresh[kind] = true
		c.toolsMu.Unlock()
		return
	}
	c.refreshing[kind] = true
	c.toolsMu.Unlock()

	go func() {
		for {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			err := list(ctx)
			cancel()
			c.toolsMu.Lock()
			fn := c.onTools
			again := c.pendingRefresh[kind]
			delete(c.pendingRefresh, kind)
			if !again {
				delete(c.refreshing, kind)
			}
			c.toolsMu.Unlock()
			if err == nil && fn != nil {
				fn()
			}
			if !again {
				return
			}
		}
	}()
}
