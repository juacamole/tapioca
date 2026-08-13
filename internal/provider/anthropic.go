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
	apiKey  string // empty when authenticating by OAuth
	oauth   bool
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
	base := strings.TrimSuffix(cfg.BaseURL, "/")
	if base == "" {
		base = "https://api.anthropic.com"
	}
	// An explicit oauth mode authenticates from the CLI profile instead of a
	// key. A key still wins when both are configured, matching how the SDKs
	// resolve credentials — but the connect screen reports that, because a
	// login that silently does nothing is the worst version of this.
	if cfg.Auth == "oauth" && key == "" {
		return &Anthropic{name: name, baseURL: base, oauth: true, client: httpClient}, nil
	}
	if key == "" {
		return nil, fmt.Errorf("anthropic: no API key (set providers.%s.api_key or $%s)", name, env)
	}
	return &Anthropic{name: name, baseURL: base, apiKey: key, client: httpClient}, nil
}

func (a *Anthropic) Name() string { return a.name }

// authorize sets the credential headers. OAuth and key auth differ only here:
// a bearer token on Authorization plus the oauth beta header, rather than
// x-api-key. Both paths run through this so a new request site cannot be
// written that authenticates one way and not the other.
func (a *Anthropic) authorize(ctx context.Context, h http.Header) error {
	h.Set("anthropic-version", "2023-06-01")
	if !a.oauth {
		h.Set("x-api-key", a.apiKey)
		return nil
	}
	tok, err := oauthToken(ctx)
	if err != nil {
		return err
	}
	h.Set("Authorization", "Bearer "+tok)
	h.Set("anthropic-beta", "oauth-2025-04-20")
	return nil
}

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
	Type string `json:"type"`
	// Omitted for adaptive and disabled: a budget_tokens of any value, zero
	// included, is rejected by the models that take those modes.
	BudgetTokens int `json:"budget_tokens,omitempty"`
}

// anthOutputConfig carries the effort level on models that take one.
type anthOutputConfig struct {
	Effort string `json:"effort,omitempty"`
}

type anthReq struct {
	Model            string            `json:"model,omitempty"`
	AnthropicVersion string            `json:"anthropic_version,omitempty"`
	MaxTokens        int               `json:"max_tokens"`
	System           []anthBlock       `json:"system,omitempty"`
	Messages         []anthMsg         `json:"messages"`
	Stream           bool              `json:"stream"`
	Temperature      *float64          `json:"temperature,omitempty"`
	Thinking         *anthThinking     `json:"thinking,omitempty"`
	OutputConfig     *anthOutputConfig `json:"output_config,omitempty"`
	Tools            []anthTool        `json:"tools,omitempty"`
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
	if err := a.authorize(ctx, httpReq.Header); err != nil {
		return Message{}, err
	}

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
	if err := a.authorize(ctx, req.Header); err != nil {
		return nil, err
	}
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
	applyThinking(&body, req)

	return body
}

// applyThinking sets the thinking, effort and sampling fields for whatever the
// named model accepts. Each generation removed parameters rather than
// deprecating them, so sending the previous shape is a rejected request.
func applyThinking(body *anthReq, req Request) {
	caps := anthCapsFor(req.Model)

	if req.Thinking {
		budget := req.ThinkingBudget
		if budget < 1024 {
			budget = 1024
		}
		switch {
		case caps.adaptive:
			body.Thinking = &anthThinking{Type: "adaptive"}
			if caps.effort {
				body.OutputConfig = &anthOutputConfig{Effort: effortFor(budget)}
			}
		case caps.budget:
			// The budget is a ceiling inside max_tokens, so the response needs
			// room for an answer beyond it.
			if body.MaxTokens <= budget {
				body.MaxTokens = budget + 1024
			}
			body.Thinking = &anthThinking{Type: "enabled", BudgetTokens: budget}
		}
		// Sampling is rejected alongside thinking on every model that takes
		// it, and had to be 1 on the ones where it was allowed.
		return
	}

	// Off. Omitting the field is not off on the current models — they think by
	// default — so it has to be said, in the spelling each one accepts. Fable
	// and Mythos have no off at all, and rejecting an explicit disabled is
	// their way of saying so.
	if caps.canDisable {
		body.Thinking = &anthThinking{Type: "disabled"}
	}
	if caps.sampling && req.Temperature >= 0 {
		t := req.Temperature
		body.Temperature = &t
	}
}
