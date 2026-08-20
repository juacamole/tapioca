// Package provider defines the streaming LLM backend interface and the
// message/block model shared across the app.
package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"tapioca/internal/config"
)

// Block is one piece of message content. The union of fields covers text,
// thinking, tool_use and tool_result blocks so providers can replay history
// with full fidelity.
type Block struct {
	Type      string          `json:"type"` // "text" | "thinking" | "tool_use" | "tool_result" | "image"
	Text      string          `json:"text,omitempty"`
	Signature string          `json:"signature,omitempty"`   // thinking signature (anthropic)
	ID        string          `json:"id,omitempty"`          // tool_use id
	Name      string          `json:"name,omitempty"`        // tool name (tool_use and tool_result)
	Input     json.RawMessage `json:"input,omitempty"`       // tool_use arguments
	ToolUseID string          `json:"tool_use_id,omitempty"` // tool_result linkage
	Content   string          `json:"content,omitempty"`     // tool_result payload
	IsError   bool            `json:"is_error,omitempty"`
	MediaType string          `json:"media_type,omitempty"` // image mime
	Data      string          `json:"data,omitempty"`       // image base64
	// Display is shown in the transcript instead of Content when set (a file
	// diff, say). Providers build their payloads from the fields above, so it
	// is saved with the session but never sent to the model.
	Display string `json:"display,omitempty"`
}

// Message is one conversation turn. Hidden messages reach the model but are
// not rendered in the transcript (e.g. rewind notices). Typed preserves the
// user's raw input before mention expansion, for /edit.
type Message struct {
	Role   string    `json:"role"` // "user" | "assistant"
	Blocks []Block   `json:"blocks"`
	Model  string    `json:"model,omitempty"`
	Usage  *Usage    `json:"usage,omitempty"`
	Hidden bool      `json:"hidden,omitempty"`
	Typed  string    `json:"typed,omitempty"`
	Time   time.Time `json:"time"`
}

// Text returns the concatenated text blocks.
func (m *Message) Text() string {
	var b strings.Builder
	for _, bl := range m.Blocks {
		if bl.Type == "text" {
			b.WriteString(bl.Text)
		}
	}
	return b.String()
}

// Thinking returns the concatenated thinking blocks.
func (m *Message) Thinking() string {
	var b strings.Builder
	for _, bl := range m.Blocks {
		if bl.Type == "thinking" {
			if b.Len() > 0 {
				b.WriteString("\n")
			}
			b.WriteString(bl.Text)
		}
	}
	return b.String()
}

// ToolUses returns the tool_use blocks.
func (m *Message) ToolUses() []Block {
	var out []Block
	for _, bl := range m.Blocks {
		if bl.Type == "tool_use" {
			out = append(out, bl)
		}
	}
	return out
}

// IsToolResult reports whether the message only carries tool results.
func (m *Message) IsToolResult() bool {
	if len(m.Blocks) == 0 {
		return false
	}
	for _, bl := range m.Blocks {
		if bl.Type != "tool_result" {
			return false
		}
	}
	return true
}

// TextMessage builds a plain text message.
func TextMessage(role, text string) Message {
	return Message{
		Role:   role,
		Blocks: []Block{{Type: "text", Text: text}},
		Time:   time.Now(),
	}
}

// Usage counts tokens for one request. Cache counts are reported separately
// from input by Anthropic.
type Usage struct {
	InputTokens      int `json:"input_tokens"`
	OutputTokens     int `json:"output_tokens"`
	CacheReadTokens  int `json:"cache_read_tokens,omitempty"`
	CacheWriteTokens int `json:"cache_write_tokens,omitempty"`
}

// ToolDef describes a callable tool offered to the model.
type ToolDef struct {
	Name        string
	Description string
	InputSchema json.RawMessage
}

// EventType enumerates streaming events.
type EventType int

const (
	EventTextDelta EventType = iota
	EventThinkingDelta
	EventThinkingDone // the thinking block closed (before the answer starts)
	EventToolUseStart
	EventToolInputDelta
	EventUsage
	EventDone
)

// Event is one streaming update emitted while a response is generated.
type Event struct {
	Type       EventType
	Text       string // delta payload
	ToolID     string
	ToolName   string
	Usage      Usage
	StopReason string
}

// Request is a single chat completion request.
type Request struct {
	Model          string
	System         string
	Messages       []Message
	MaxTokens      int
	Temperature    float64
	Thinking       bool
	ThinkingBudget int
	Tools          []ToolDef
}

// Provider streams chat completions.
type Provider interface {
	Name() string
	// Stream emits events on out while generating and returns the assembled
	// assistant message. It closes out before returning. On context
	// cancellation it returns the partial message together with ctx.Err().
	Stream(ctx context.Context, req Request, out chan<- Event) (Message, error)
	// ListModels returns model names available on this provider.
	ListModels(ctx context.Context) ([]string, error)
}

// New builds a provider from its config entry.
func New(name string, cfg config.ProviderConfig) (Provider, error) {
	switch cfg.Type {
	case "ollama", "":
		return NewOllama(name, cfg), nil
	case "llamacpp", "llama.cpp", "llama-cpp":
		return NewLlamaCpp(name, cfg), nil
	case "anthropic":
		return NewAnthropic(name, cfg)
	case "openai", "openai-compatible":
		return NewOpenAI(name, cfg), nil
	case "custom":
		return NewCustom(name, cfg)
	case "vercel":
		return NewVercel(name, cfg), nil
	case "azure":
		return NewAzure(name, cfg)
	case "gemini", "google":
		return NewGemini(name, cfg), nil
	case "bedrock":
		return NewBedrock(name, cfg)
	case "vertex":
		return NewVertex(name, cfg)
	default:
		return nil, fmt.Errorf("provider %q: unknown type %q", name, cfg.Type)
	}
}

// Identifier is a provider that can confirm the server it points at is really
// the kind of server it was configured as, by asking for something only that
// server serves. Looking for a local model server nobody configured means
// knocking on a port that belongs to whoever got there first, so "something
// answered" is not an answer: a provider that cannot prove what it reached is
// never reported as found.
type Identifier interface {
	Identify(ctx context.Context) error
}

// IsLocal reports whether a provider type runs inference on this machine —
// free, and serving model names that must never match hosted catalog ids.
func IsLocal(typ string) bool {
	switch typ {
	case "", "ollama", "llamacpp", "llama.cpp", "llama-cpp":
		return true
	}
	return false
}
