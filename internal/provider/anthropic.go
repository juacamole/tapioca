package provider

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"

	"tapioca/internal/config"
)

// Anthropic streams from the Anthropic Messages API, including extended
// thinking and tool use.
type Anthropic struct {
	name    string
	baseURL string
	apiKey  string
	client  *http.Client
}

// NewAnthropic builds the provider, resolving the API key from config or env.
func NewAnthropic(name string, cfg config.ProviderConfig) (*Anthropic, error) {
	key := cfg.APIKey
	env := cfg.APIKeyEnv
	if env == "" {
		env = "ANTHROPIC_API_KEY"
	}
	if key == "" {
		key = os.Getenv(env)
	}
	if key == "" {
		return nil, fmt.Errorf("anthropic: no API key (set providers.%s.api_key or $%s)", name, env)
	}
	base := strings.TrimSuffix(cfg.BaseURL, "/")
	if base == "" {
		base = "https://api.anthropic.com"
	}
	return &Anthropic{name: name, baseURL: base, apiKey: key, client: httpClient}, nil
}

func (a *Anthropic) Name() string { return a.name }

type cacheControl struct {
	Type string `json:"type"`
}

var ephemeral = &cacheControl{Type: "ephemeral"}

type anthBlock struct {
	Type         string          `json:"type"`
	Text         string          `json:"text,omitempty"`
	Thinking     string          `json:"thinking,omitempty"`
	Signature    string          `json:"signature,omitempty"`
	ID           string          `json:"id,omitempty"`
	Name         string          `json:"name,omitempty"`
	Input        json.RawMessage `json:"input,omitempty"`
	ToolUseID    string          `json:"tool_use_id,omitempty"`
	Content      string          `json:"content,omitempty"`
	IsError      bool            `json:"is_error,omitempty"`
	CacheControl *cacheControl   `json:"cache_control,omitempty"`
}

type anthMsg struct {
	Role    string      `json:"role"`
	Content []anthBlock `json:"content"`
}

type anthTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema"`
}

type anthThinking struct {
	Type         string `json:"type"`
	BudgetTokens int    `json:"budget_tokens"`
}

type anthReq struct {
	Model       string        `json:"model"`
	MaxTokens   int           `json:"max_tokens"`
	System      []anthBlock   `json:"system,omitempty"`
	Messages    []anthMsg     `json:"messages"`
	Stream      bool          `json:"stream"`
	Temperature *float64      `json:"temperature,omitempty"`
	Thinking    *anthThinking `json:"thinking,omitempty"`
	Tools       []anthTool    `json:"tools,omitempty"`
}

type anthUsage struct {
	InputTokens         int `json:"input_tokens"`
	OutputTokens        int `json:"output_tokens"`
	CacheReadTokens     int `json:"cache_read_input_tokens"`
	CacheCreationTokens int `json:"cache_creation_input_tokens"`
}

type sseEvent struct {
	Type    string `json:"type"`
	Index   int    `json:"index"`
	Message *struct {
		Model string    `json:"model"`
		Usage anthUsage `json:"usage"`
	} `json:"message"`
	ContentBlock *struct {
		Type string `json:"type"`
		ID   string `json:"id"`
		Name string `json:"name"`
	} `json:"content_block"`
	Delta *struct {
		Type        string `json:"type"`
		Text        string `json:"text"`
		Thinking    string `json:"thinking"`
		Signature   string `json:"signature"`
		PartialJSON string `json:"partial_json"`
		StopReason  string `json:"stop_reason"`
	} `json:"delta"`
	Usage *anthUsage `json:"usage"`
	Error *struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}

func (a *Anthropic) convertMessages(model string, msgs []Message) []anthMsg {
	var out []anthMsg
	for _, m := range msgs {
		if m.Role != "user" && m.Role != "assistant" {
			continue // local notes etc. never reach the model
		}
		var blocks []anthBlock
		for _, b := range m.Blocks {
			switch b.Type {
			case "text":
				if strings.TrimSpace(b.Text) != "" {
					blocks = append(blocks, anthBlock{Type: "text", Text: b.Text})
				}
			case "thinking":
				// Signatures are model-specific; replay only for the model
				// that produced them.
				if b.Signature != "" && (m.Model == "" || m.Model == model) {
					blocks = append(blocks, anthBlock{Type: "thinking", Thinking: b.Text, Signature: b.Signature})
				}
			case "tool_use":
				input := b.Input
				if len(input) == 0 {
					input = json.RawMessage("{}")
				}
				blocks = append(blocks, anthBlock{Type: "tool_use", ID: b.ID, Name: b.Name, Input: input})
			case "tool_result":
				blocks = append(blocks, anthBlock{Type: "tool_result", ToolUseID: b.ToolUseID, Content: b.Content, IsError: b.IsError})
			}
		}
		if len(blocks) == 0 {
			continue
		}
		// Merge consecutive same-role messages: the API requires alternation.
		if len(out) > 0 && out[len(out)-1].Role == m.Role {
			out[len(out)-1].Content = append(out[len(out)-1].Content, blocks...)
			continue
		}
		out = append(out, anthMsg{Role: m.Role, Content: blocks})
	}
	return out
}

// Stream implements Provider.
func (a *Anthropic) Stream(ctx context.Context, req Request, out chan<- Event) (Message, error) {
	defer close(out)

	body := anthReq{
		Model:     req.Model,
		MaxTokens: req.MaxTokens,
		Messages:  a.convertMessages(req.Model, req.Messages),
		Stream:    true,
	}
	if req.System != "" {
		// Marking the system prompt caches the whole prefix including tools.
		body.System = []anthBlock{{Type: "text", Text: req.System, CacheControl: ephemeral}}
	}
	for _, t := range req.Tools {
		schema := t.InputSchema
		if len(schema) == 0 {
			schema = json.RawMessage(`{"type":"object"}`)
		}
		body.Tools = append(body.Tools, anthTool{Name: t.Name, Description: t.Description, InputSchema: schema})
	}
	sort.Slice(body.Tools, func(i, j int) bool { return body.Tools[i].Name < body.Tools[j].Name })
	// Breakpoints on the last two messages make the growing conversation
	// prefix cacheable turn over turn.
	marked := 0
	for i := len(body.Messages) - 1; i >= 0 && marked < 2; i-- {
		blocks := body.Messages[i].Content
		if len(blocks) == 0 {
			continue
		}
		blocks[len(blocks)-1].CacheControl = ephemeral
		marked++
	}
	if req.Thinking {
		budget := req.ThinkingBudget
		if budget < 1024 {
			budget = 1024
		}
		if body.MaxTokens <= budget {
			body.MaxTokens = budget + 1024
		}
		body.Thinking = &anthThinking{Type: "enabled", BudgetTokens: budget}
		// Temperature must be 1 with thinking enabled; omit it.
	} else if req.Temperature >= 0 {
		t := req.Temperature
		body.Temperature = &t
	}

	payload, err := json.Marshal(body)
	if err != nil {
		return Message{}, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, "POST", a.baseURL+"/v1/messages", bytes.NewReader(payload))
	if err != nil {
		return Message{}, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("x-api-key", a.apiKey)
	httpReq.Header.Set("anthropic-version", "2023-06-01")

	resp, err := a.client.Do(httpReq)
	if err != nil {
		return Message{}, fmt.Errorf("anthropic: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return Message{}, newAPIError(a.name, resp, data)
	}

	type blockBuilder struct {
		typ       string
		id, name  string
		text      strings.Builder
		signature string
		inputJSON strings.Builder
	}
	builders := map[int]*blockBuilder{}
	var usage Usage
	var stopReason string
	msg := Message{Role: "assistant", Model: req.Model, Time: time.Now()}

	finish := func() Message {
		idxs := make([]int, 0, len(builders))
		for i := range builders {
			idxs = append(idxs, i)
		}
		sort.Ints(idxs)
		for _, i := range idxs {
			b := builders[i]
			switch b.typ {
			case "text":
				msg.Blocks = append(msg.Blocks, Block{Type: "text", Text: b.text.String()})
			case "thinking":
				msg.Blocks = append(msg.Blocks, Block{Type: "thinking", Text: b.text.String(), Signature: b.signature})
			case "tool_use":
				input := b.inputJSON.String()
				if strings.TrimSpace(input) == "" {
					input = "{}"
				}
				msg.Blocks = append(msg.Blocks, Block{Type: "tool_use", ID: b.id, Name: b.name, Input: json.RawMessage(input)})
			}
		}
		if usage.InputTokens > 0 || usage.OutputTokens > 0 || usage.CacheReadTokens > 0 || usage.CacheWriteTokens > 0 {
			u := usage
			msg.Usage = &u
		}
		return msg
	}

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "" {
			continue
		}
		var ev sseEvent
		if err := json.Unmarshal([]byte(data), &ev); err != nil {
			continue
		}
		switch ev.Type {
		case "message_start":
			if ev.Message != nil {
				usage.InputTokens = ev.Message.Usage.InputTokens
				usage.OutputTokens = ev.Message.Usage.OutputTokens
				usage.CacheReadTokens = ev.Message.Usage.CacheReadTokens
				usage.CacheWriteTokens = ev.Message.Usage.CacheCreationTokens
			}
		case "content_block_start":
			if ev.ContentBlock != nil {
				builders[ev.Index] = &blockBuilder{typ: ev.ContentBlock.Type, id: ev.ContentBlock.ID, name: ev.ContentBlock.Name}
				if ev.ContentBlock.Type == "tool_use" {
					out <- Event{Type: EventToolUseStart, ToolID: ev.ContentBlock.ID, ToolName: ev.ContentBlock.Name}
				}
			}
		case "content_block_delta":
			b := builders[ev.Index]
			if b == nil || ev.Delta == nil {
				continue
			}
			switch ev.Delta.Type {
			case "text_delta":
				b.text.WriteString(ev.Delta.Text)
				out <- Event{Type: EventTextDelta, Text: ev.Delta.Text}
			case "thinking_delta":
				b.text.WriteString(ev.Delta.Thinking)
				out <- Event{Type: EventThinkingDelta, Text: ev.Delta.Thinking}
			case "signature_delta":
				b.signature += ev.Delta.Signature
			case "input_json_delta":
				b.inputJSON.WriteString(ev.Delta.PartialJSON)
				out <- Event{Type: EventToolInputDelta, Text: ev.Delta.PartialJSON, ToolID: b.id, ToolName: b.name}
			}
		case "content_block_stop":
			if b := builders[ev.Index]; b != nil && b.typ == "thinking" {
				out <- Event{Type: EventThinkingDone}
			}
		case "message_delta":
			if ev.Delta != nil && ev.Delta.StopReason != "" {
				stopReason = ev.Delta.StopReason
			}
			if ev.Usage != nil {
				usage.OutputTokens = ev.Usage.OutputTokens
			}
		case "error":
			m, typ := "stream error", ""
			if ev.Error != nil {
				m, typ = ev.Error.Message, ev.Error.Type
			}
			status := 400
			if typ == "overloaded_error" || typ == "api_error" {
				status = 529 // in-band server trouble is retryable
			}
			return finish(), &APIError{Provider: a.name, Status: status, Message: m}
		case "message_stop":
			out <- Event{Type: EventUsage, Usage: usage}
			out <- Event{Type: EventDone, StopReason: stopReason}
			return finish(), nil
		}
	}
	if ctx.Err() != nil {
		return finish(), ctx.Err()
	}
	if err := scanner.Err(); err != nil {
		return finish(), fmt.Errorf("anthropic: reading stream: %w", err)
	}
	out <- Event{Type: EventDone, StopReason: stopReason}
	return finish(), nil
}

// ListModels implements Provider.
func (a *Anthropic) ListModels(ctx context.Context) ([]string, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", a.baseURL+"/v1/models?limit=100", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("x-api-key", a.apiKey)
	req.Header.Set("anthropic-version", "2023-06-01")
	resp, err := a.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("anthropic: HTTP %d: %s", resp.StatusCode, apiErrorText(data))
	}
	var body struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, err
	}
	models := make([]string, 0, len(body.Data))
	for _, m := range body.Data {
		models = append(models, m.ID)
	}
	return models, nil
}

func apiErrorText(data []byte) string {
	var e struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(data, &e) == nil && e.Error.Message != "" {
		return e.Error.Message
	}
	s := strings.TrimSpace(string(data))
	if s == "" {
		return "unknown error"
	}
	return s
}
