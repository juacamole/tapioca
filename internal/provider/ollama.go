package provider

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"tapioca/internal/config"
)

// Ollama streams from a local Ollama server via /api/chat.
type Ollama struct {
	name      string
	baseURL   string
	ctxWindow int
	client    *http.Client
}

// NewOllama builds the provider. The default base URL is localhost:11434.
func NewOllama(name string, cfg config.ProviderConfig) *Ollama {
	base := strings.TrimSuffix(cfg.BaseURL, "/")
	if base == "" {
		base = "http://localhost:11434"
	}
	return &Ollama{name: name, baseURL: base, ctxWindow: cfg.ContextWindow, client: httpClient}
}

func (o *Ollama) Name() string { return o.name }

type olFunc struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments,omitempty"`
}

type olToolCall struct {
	Function olFunc `json:"function"`
}

type olMsg struct {
	Role      string       `json:"role"`
	Content   string       `json:"content"`
	Thinking  string       `json:"thinking,omitempty"`
	ToolName  string       `json:"tool_name,omitempty"`
	ToolCalls []olToolCall `json:"tool_calls,omitempty"`
}

type olTool struct {
	Type     string `json:"type"`
	Function struct {
		Name        string          `json:"name"`
		Description string          `json:"description,omitempty"`
		Parameters  json.RawMessage `json:"parameters"`
	} `json:"function"`
}

type olReq struct {
	Model    string         `json:"model"`
	Messages []olMsg        `json:"messages"`
	Stream   bool           `json:"stream"`
	Think    *bool          `json:"think,omitempty"`
	Tools    []olTool       `json:"tools,omitempty"`
	Options  map[string]any `json:"options,omitempty"`
}

type olChunk struct {
	Message struct {
		Role      string       `json:"role"`
		Content   string       `json:"content"`
		Thinking  string       `json:"thinking"`
		ToolCalls []olToolCall `json:"tool_calls"`
	} `json:"message"`
	Done            bool   `json:"done"`
	DoneReason      string `json:"done_reason"`
	PromptEvalCount int    `json:"prompt_eval_count"`
	EvalCount       int    `json:"eval_count"`
	Error           string `json:"error"`
}

func (o *Ollama) convertMessages(system string, msgs []Message) []olMsg {
	var out []olMsg
	if system != "" {
		out = append(out, olMsg{Role: "system", Content: system})
	}
	for _, m := range msgs {
		if m.Role != "user" && m.Role != "assistant" {
			continue // local notes etc. never reach the model
		}
		if m.IsToolResult() {
			for _, b := range m.Blocks {
				out = append(out, olMsg{Role: "tool", Content: b.Content, ToolName: b.Name})
			}
			continue
		}
		om := olMsg{Role: m.Role, Content: m.Text()}
		if m.Role == "assistant" {
			for _, b := range m.ToolUses() {
				args := b.Input
				if len(args) == 0 {
					args = json.RawMessage("{}")
				}
				om.ToolCalls = append(om.ToolCalls, olToolCall{Function: olFunc{Name: b.Name, Arguments: args}})
			}
		}
		if om.Content == "" && len(om.ToolCalls) == 0 {
			continue
		}
		out = append(out, om)
	}
	return out
}

// Stream implements Provider.
func (o *Ollama) Stream(ctx context.Context, req Request, out chan<- Event) (Message, error) {
	defer close(out)

	body := olReq{
		Model:    req.Model,
		Messages: o.convertMessages(req.System, req.Messages),
		Stream:   true,
		Options:  map[string]any{},
	}
	if req.MaxTokens > 0 {
		body.Options["num_predict"] = req.MaxTokens
	}
	// Ollama defaults num_ctx to ~4k and silently truncates the prompt.
	body.Options["num_ctx"] = o.numCtx(body.Messages, req.Tools)
	if req.Temperature >= 0 {
		body.Options["temperature"] = req.Temperature
	}
	if req.Thinking {
		t := true
		body.Think = &t
	}
	for _, td := range req.Tools {
		var t olTool
		t.Type = "function"
		t.Function.Name = td.Name
		t.Function.Description = td.Description
		schema := td.InputSchema
		if len(schema) == 0 {
			schema = json.RawMessage(`{"type":"object"}`)
		}
		t.Function.Parameters = schema
		body.Tools = append(body.Tools, t)
	}

	resp, err := o.post(ctx, body)
	if err != nil && body.Think != nil && strings.Contains(err.Error(), "think") {
		// Model doesn't support thinking; retry without it.
		body.Think = nil
		resp, err = o.post(ctx, body)
	}
	if err != nil {
		return Message{}, err
	}
	defer resp.Body.Close()

	msg := Message{Role: "assistant", Model: req.Model, Time: time.Now()}
	var text, thinking strings.Builder
	var toolCalls []olToolCall
	var usage Usage
	stopReason := ""

	finish := func() Message {
		if thinking.Len() > 0 {
			msg.Blocks = append(msg.Blocks, Block{Type: "thinking", Text: thinking.String()})
		}
		if text.Len() > 0 {
			msg.Blocks = append(msg.Blocks, Block{Type: "text", Text: text.String()})
		}
		for i, tc := range toolCalls {
			args := tc.Function.Arguments
			if len(args) == 0 {
				args = json.RawMessage("{}")
			}
			msg.Blocks = append(msg.Blocks, Block{
				Type:  "tool_use",
				ID:    fmt.Sprintf("call_%d_%d", time.Now().UnixMilli(), i),
				Name:  tc.Function.Name,
				Input: args,
			})
		}
		if usage.InputTokens > 0 || usage.OutputTokens > 0 {
			u := usage
			msg.Usage = &u
		}
		return msg
	}

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var ch olChunk
		if err := json.Unmarshal([]byte(line), &ch); err != nil {
			continue
		}
		if ch.Error != "" {
			return finish(), fmt.Errorf("ollama: %s", ch.Error)
		}
		if ch.Message.Thinking != "" {
			thinking.WriteString(ch.Message.Thinking)
			out <- Event{Type: EventThinkingDelta, Text: ch.Message.Thinking}
		}
		if ch.Message.Content != "" {
			text.WriteString(ch.Message.Content)
			out <- Event{Type: EventTextDelta, Text: ch.Message.Content}
		}
		for _, tc := range ch.Message.ToolCalls {
			toolCalls = append(toolCalls, tc)
			out <- Event{Type: EventToolUseStart, ToolName: tc.Function.Name}
		}
		if ch.Done {
			usage.InputTokens = ch.PromptEvalCount
			usage.OutputTokens = ch.EvalCount
			stopReason = ch.DoneReason
			if len(toolCalls) > 0 {
				stopReason = "tool_use"
			}
			out <- Event{Type: EventUsage, Usage: usage}
			out <- Event{Type: EventDone, StopReason: stopReason}
			return finish(), nil
		}
	}
	if ctx.Err() != nil {
		return finish(), ctx.Err()
	}
	if err := scanner.Err(); err != nil {
		return finish(), fmt.Errorf("ollama: reading stream: %w", err)
	}
	out <- Event{Type: EventDone, StopReason: stopReason}
	return finish(), nil
}

func (o *Ollama) numCtx(msgs []olMsg, toolDefs []ToolDef) int {
	chars := 0
	for _, m := range msgs {
		chars += len(m.Content) + len(m.Thinking)
		for _, tc := range m.ToolCalls {
			chars += len(tc.Function.Name) + len(tc.Function.Arguments)
		}
	}
	for _, t := range toolDefs {
		chars += len(t.Name) + len(t.Description) + len(t.InputSchema)
	}
	est := chars / 4
	n := est + est/4 + 8192
	if o.ctxWindow > 0 && n > o.ctxWindow {
		n = o.ctxWindow
	}
	return n
}

func (o *Ollama) post(ctx context.Context, body olReq) (*http.Response, error) {
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, "POST", o.baseURL+"/api/chat", bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	resp, err := o.client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("ollama: %w (is ollama running at %s?)", err, o.baseURL)
	}
	if resp.StatusCode != http.StatusOK {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		resp.Body.Close()
		var e struct {
			Error string `json:"error"`
		}
		m := strings.TrimSpace(string(data))
		if json.Unmarshal(data, &e) == nil && e.Error != "" {
			m = e.Error
		}
		ae := newAPIError(o.name, resp, data)
		ae.Message = m
		return nil, ae
	}
	return resp, nil
}

// ListModels implements Provider.
func (o *Ollama) ListModels(ctx context.Context) ([]string, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", o.baseURL+"/api/tags", nil)
	if err != nil {
		return nil, err
	}
	resp, err := o.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("ollama: %w (is ollama running at %s?)", err, o.baseURL)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("ollama: HTTP %d", resp.StatusCode)
	}
	var body struct {
		Models []struct {
			Name string `json:"name"`
		} `json:"models"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, err
	}
	models := make([]string, 0, len(body.Models))
	for _, m := range body.Models {
		models = append(models, m.Name)
	}
	return models, nil
}
