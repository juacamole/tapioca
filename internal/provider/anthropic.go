package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"

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

type anthSource struct {
	Type      string `json:"type"`
	MediaType string `json:"media_type"`
	Data      string `json:"data"`
}

type anthBlock struct {
	Type         string          `json:"type"`
	Text         string          `json:"text,omitempty"`
	Thinking     string          `json:"thinking,omitempty"`
	Signature    string          `json:"signature,omitempty"`
	Data         string          `json:"data,omitempty"`
	ID           string          `json:"id,omitempty"`
	Name         string          `json:"name,omitempty"`
	Input        json.RawMessage `json:"input,omitempty"`
	ToolUseID    string          `json:"tool_use_id,omitempty"`
	Content      string          `json:"content,omitempty"`
	IsError      bool            `json:"is_error,omitempty"`
	Source       *anthSource     `json:"source,omitempty"`
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
	Model            string        `json:"model,omitempty"`
	AnthropicVersion string        `json:"anthropic_version,omitempty"`
	MaxTokens        int           `json:"max_tokens"`
	System           []anthBlock   `json:"system,omitempty"`
	Messages         []anthMsg     `json:"messages"`
	Stream           bool          `json:"stream"`
	Temperature      *float64      `json:"temperature,omitempty"`
	Thinking         *anthThinking `json:"thinking,omitempty"`
	Tools            []anthTool    `json:"tools,omitempty"`
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
		Data string `json:"data"`
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
			case "redacted_thinking":
				if b.Data != "" && (m.Model == "" || m.Model == model) {
					blocks = append(blocks, anthBlock{Type: "redacted_thinking", Data: b.Data})
				}
			case "tool_use":
				input := b.Input
				if len(input) == 0 {
					input = json.RawMessage("{}")
				}
				blocks = append(blocks, anthBlock{Type: "tool_use", ID: b.ID, Name: b.Name, Input: input})
			case "tool_result":
				blocks = append(blocks, anthBlock{Type: "tool_result", ToolUseID: b.ToolUseID, Content: b.Content, IsError: b.IsError})
			case "image":
				if b.Data != "" {
					blocks = append(blocks, anthBlock{Type: "image", Source: &anthSource{Type: "base64", MediaType: b.MediaType, Data: b.Data}})
				}
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

	body := a.buildBody(req)
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

	return a.streamAnthropicSSE(ctx, req.Model, resp.Body, out)
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

// buildBody assembles the Messages-API request. Bedrock and Vertex send
// the same shape, so they build it here and adjust the few fields their
// transports move into the URL.
func (a *Anthropic) buildBody(req Request) anthReq {
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

	return body
}
