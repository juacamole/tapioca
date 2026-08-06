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
	Type      string          `json:"type"` // "text" | "thinking" | "tool_use" | "tool_result"
	Text      string          `json:"text,omitempty"`
	Signature string          `json:"signature,omitempty"`   // thinking signature (anthropic)
	ID        string          `json:"id,omitempty"`          // tool_use id
	Name      string          `json:"name,omitempty"`        // tool name (tool_use and tool_result)
	Input     json.RawMessage `json:"input,omitempty"`       // tool_use arguments
	ToolUseID string          `json:"tool_use_id,omitempty"` // tool_result linkage
	Content   string          `json:"content,omitempty"`     // tool_result payload
	IsError   bool            `json:"is_error,omitempty"`
}

// Message is one conversation turn. Hidden messages reach the model but are
// not rendered in the transcript (e.g. rewind notices).
type Message struct {
	Role   string    `json:"role"` // "user" | "assistant"
	Blocks []Block   `json:"blocks"`
	Model  string    `json:"model,omitempty"`
	Usage  *Usage    `json:"usage,omitempty"`
	Hidden bool      `json:"hidden,omitempty"`
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
	case "anthropic":
		return NewAnthropic(name, cfg)
	case "openai", "openai-compatible":
		return NewOpenAI(name, cfg), nil
	default:
		return nil, fmt.Errorf("provider %q: unknown type %q", name, cfg.Type)
	}
}
