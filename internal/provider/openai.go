package provider

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"tapioca/internal/config"
)

// OpenAI streams from any OpenAI-compatible chat completions endpoint:
// OpenAI itself, LM Studio, vLLM, llama.cpp server, OpenRouter, and — through
// the flavours below — Azure and Gemini, which speak the same protocol behind
// different URLs and a different auth header.
type OpenAI struct {
	name       string
	baseURL    string
	apiKey     string
	auth       *authSpec // set for the custom type; nil uses the flavor's own scheme
	flavor     string    // "", flavorAzure, flavorGemini
	apiVersion string    // azure only
	client     *http.Client
}

const (
	flavorAzure  = "azure"
	flavorGemini = "gemini"

	geminiBase       = "https://generativelanguage.googleapis.com/v1beta/openai"
	azureAPIVersion  = "2024-10-21"
	openAIDefaultURL = "https://api.openai.com"
)

// NewOpenAI builds the provider. The API key is optional (local servers).
func NewOpenAI(name string, cfg config.ProviderConfig) *OpenAI {
	return newOpenAILike(name, cfg, "", openAIDefaultURL, "OPENAI_API_KEY")
}

// NewAzure targets Azure OpenAI, where the model name is a deployment, the key
// travels in its own header, and the API version is part of the URL.
func NewAzure(name string, cfg config.ProviderConfig) (*OpenAI, error) {
	if strings.TrimSpace(cfg.BaseURL) == "" {
		return nil, fmt.Errorf("provider %q: azure needs base_url (https://<resource>.openai.azure.com)", name)
	}
	o := newOpenAILike(name, cfg, flavorAzure, "", "AZURE_OPENAI_API_KEY")
	o.apiVersion = cfg.APIVersion
	if o.apiVersion == "" {
		o.apiVersion = azureAPIVersion
	}
	return o, nil
}

// NewGemini targets Google's OpenAI-compatible endpoint, so Gemini works
// without a second protocol implementation.
func NewGemini(name string, cfg config.ProviderConfig) *OpenAI {
	return newOpenAILike(name, cfg, flavorGemini, geminiBase, "GEMINI_API_KEY")
}

func newOpenAILike(name string, cfg config.ProviderConfig, flavor, defaultBase, defaultEnv string) *OpenAI {
	key := cfg.APIKey
	if key == "" {
		env := cfg.APIKeyEnv
		if env == "" {
			env = defaultEnv
		}
		key = os.Getenv(env)
	}
	base := strings.TrimSuffix(cfg.BaseURL, "/")
	if base == "" {
		base = defaultBase
	}
	if flavor == "" {
		base = trimVersionSuffix(base)
	}
	return &OpenAI{name: name, baseURL: base, apiKey: key, flavor: flavor, client: httpClient}
}

// trimVersionSuffix drops a trailing /v1 from a configured base URL, because
// the default flavour appends /v1 itself. Every gateway publishes its address
// with that segment already on it — https://ai-gateway.vercel.sh/v1,
// https://openrouter.ai/api/v1, https://api.groq.com/openai/v1 — so a URL
// pasted from the provider's own documentation would otherwise request
// /v1/v1 and get a 404 that names nothing. Only the last segment goes:
// https://openrouter.ai/api/v1 becomes .../api, and /v1 is put back on.
func trimVersionSuffix(base string) string {
	return strings.TrimSuffix(strings.TrimSuffix(base, "/v1"), "/")
}

// chatURL builds the completions endpoint for this flavour.
func (o *OpenAI) chatURL(model string) string {
	switch o.flavor {
	case flavorAzure:
		return fmt.Sprintf("%s/openai/deployments/%s/chat/completions?api-version=%s",
			o.baseURL, url.PathEscape(model), url.QueryEscape(o.apiVersion))
	case flavorGemini:
		return o.baseURL + "/chat/completions" // base already ends in /openai
	default:
		return o.baseURL + "/v1/chat/completions"
	}
}

func (o *OpenAI) modelsURL() string {
	switch o.flavor {
	case flavorAzure:
		return o.baseURL + "/openai/models?api-version=" + url.QueryEscape(o.apiVersion)
	case flavorGemini:
		return o.baseURL + "/models"
	default:
		return o.baseURL + "/v1/models"
	}
}

// setAuth attaches the key the way this flavour expects. A custom provider
// carries its own spec, since the same wire format is served behind a bearer
// token, a named header, a query parameter and nothing at all.
func (o *OpenAI) setAuth(r *http.Request) error {
	if o.auth != nil {
		// Returned rather than swallowed: a request that quietly goes out
		// unauthenticated fails later, somewhere that does not name the cause.
		return o.auth.apply(r)
	}
	if o.apiKey == "" {
		return nil
	}
	if o.flavor == flavorAzure {
		r.Header.Set("api-key", o.apiKey)
		return nil
	}
	r.Header.Set("Authorization", "Bearer "+o.apiKey)
	return nil
}

func (o *OpenAI) Name() string { return o.name }

type oaToolCall struct {
	ID       string `json:"id,omitempty"`
	Type     string `json:"type,omitempty"`
	Index    *int   `json:"index,omitempty"`
	Function struct {
		Name      string `json:"name,omitempty"`
		Arguments string `json:"arguments,omitempty"`
	} `json:"function"`
}

type oaMsg struct {
	Role       string       `json:"role"`
	Content    any          `json:"content"`
	ToolCalls  []oaToolCall `json:"tool_calls,omitempty"`
	ToolCallID string       `json:"tool_call_id,omitempty"`
}

type oaReq struct {
	Model         string         `json:"model"`
	Messages      []oaMsg        `json:"messages"`
	Stream        bool           `json:"stream"`
	StreamOptions map[string]any `json:"stream_options,omitempty"`
	MaxTokens     int            `json:"max_tokens,omitempty"`
	// Reasoning models reject max_tokens and a non-default temperature, and
	// need to be told how hard to think.
	MaxCompletion   int      `json:"max_completion_tokens,omitempty"`
	ReasoningEffort string   `json:"reasoning_effort,omitempty"`
	Temperature     *float64 `json:"temperature,omitempty"`
	Tools           []oaTool `json:"tools,omitempty"`
}

// reasoningEffort maps the thinking budget onto the low/medium/high scale
// OpenAI-compatible servers expect.
func reasoningEffort(budget int) string {
	switch {
	case budget <= 1024:
		return "low"
	case budget <= 4096:
		return "medium"
	default:
		return "high"
	}
}

type oaTool struct {
	Type     string `json:"type"`
	Function struct {
		Name        string          `json:"name"`
		Description string          `json:"description,omitempty"`
		Parameters  json.RawMessage `json:"parameters"`
	} `json:"function"`
}

type oaChunk struct {
	Choices []struct {
		Delta struct {
			Content          string       `json:"content"`
			ReasoningContent string       `json:"reasoning_content"` // deepseek-style
			Reasoning        string       `json:"reasoning"`         // openrouter-style
			ToolCalls        []oaToolCall `json:"tool_calls"`
		} `json:"delta"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage *struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
	} `json:"usage"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

func (o *OpenAI) convertMessages(system string, msgs []Message) []oaMsg {
	var out []oaMsg
	if system != "" {
		out = append(out, oaMsg{Role: "system", Content: system})
	}
	for _, m := range msgs {
		if m.Role != "user" && m.Role != "assistant" {
			continue // local notes etc. never reach the model
		}
		if m.IsToolResult() {
			for _, b := range m.Blocks {
				out = append(out, oaMsg{Role: "tool", Content: b.Content, ToolCallID: b.ToolUseID})
			}
			continue
		}
		om := oaMsg{Role: m.Role, Content: m.Text()}
		var imgs []map[string]any
		for _, b := range m.Blocks {
			if b.Type == "image" && b.Data != "" {
				imgs = append(imgs, map[string]any{
					"type":      "image_url",
					"image_url": map[string]string{"url": "data:" + b.MediaType + ";base64," + b.Data},
				})
			}
		}
		if len(imgs) > 0 {
			parts := []map[string]any{{"type": "text", "text": m.Text()}}
			om.Content = append(parts, imgs...)
		}
		if m.Role == "assistant" {
			for _, b := range m.ToolUses() {
				var tc oaToolCall
				tc.ID = b.ID
				tc.Type = "function"
				tc.Function.Name = b.Name
				args := string(b.Input)
				if args == "" {
					args = "{}"
				}
				tc.Function.Arguments = args
				om.ToolCalls = append(om.ToolCalls, tc)
			}
		}
		if s, isStr := om.Content.(string); isStr && s == "" && len(om.ToolCalls) == 0 {
			continue
		}
		out = append(out, om)
	}
	return out
}

// Stream implements Provider.
func (o *OpenAI) Stream(ctx context.Context, req Request, out chan<- Event) (Message, error) {
	defer close(out)

	body := oaReq{
		Model:         req.Model,
		Messages:      o.convertMessages(req.System, req.Messages),
		Stream:        true,
		StreamOptions: map[string]any{"include_usage": true},
	}
	if req.Thinking {
		// Asking for thinking makes this a reasoning request, which has a
		// different shape: no max_tokens, no temperature.
		body.MaxCompletion = req.MaxTokens
		body.ReasoningEffort = reasoningEffort(req.ThinkingBudget)
	} else {
		body.MaxTokens = req.MaxTokens
		if req.Temperature >= 0 {
			t := req.Temperature
			body.Temperature = &t
		}
	}
	for _, td := range req.Tools {
		var t oaTool
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

	payload, err := json.Marshal(body)
	if err != nil {
		return Message{}, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, "POST", o.chatURL(req.Model), bytes.NewReader(payload))
	if err != nil {
		return Message{}, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if err := o.setAuth(httpReq); err != nil {
		return Message{}, fmt.Errorf("%s: %w", o.name, err)
	}
	resp, err := o.client.Do(httpReq)
	if err != nil {
		return Message{}, fmt.Errorf("%s: %w", o.name, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return Message{}, newAPIError(o.name, resp, data)
	}

	msg := Message{Role: "assistant", Model: req.Model, Time: time.Now()}
	var text, thinking strings.Builder
	type tcBuild struct {
		id   string
		name string
		args strings.Builder
	}
	toolCalls := map[int]*tcBuild{}
	order := []int{}
	var usage Usage
	stopReason := ""

	finish := func() Message {
		if thinking.Len() > 0 {
			msg.Blocks = append(msg.Blocks, Block{Type: "thinking", Text: thinking.String()})
		}
		if text.Len() > 0 {
			msg.Blocks = append(msg.Blocks, Block{Type: "text", Text: text.String()})
		}
		for _, i := range order {
			tc := toolCalls[i]
			args := strings.TrimSpace(tc.args.String())
			if args == "" || !json.Valid([]byte(args)) {
				args = "{}"
			}
			id := tc.id
			if id == "" {
				id = fmt.Sprintf("call_%d_%d", time.Now().UnixMilli(), i)
			}
			msg.Blocks = append(msg.Blocks, Block{Type: "tool_use", ID: id, Name: tc.name, Input: json.RawMessage(args)})
		}
		if usage.InputTokens > 0 || usage.OutputTokens > 0 {
			u := usage
			msg.Usage = &u
		}
		return msg
	}

	streamed := 0
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
		if data == "[DONE]" {
			out <- Event{Type: EventUsage, Usage: usage}
			out <- Event{Type: EventDone, StopReason: stopReason}
			return finish(), nil
		}
		var ch oaChunk
		if err := json.Unmarshal([]byte(data), &ch); err != nil {
			continue
		}
		if ch.Error != nil {
			return finish(), fmt.Errorf("%s: %s", o.name, ch.Error.Message)
		}
		if ch.Usage != nil {
			usage.InputTokens = ch.Usage.PromptTokens
			usage.OutputTokens = ch.Usage.CompletionTokens
		}
		for _, c := range ch.Choices {
			streamed += len(c.Delta.Content) + len(c.Delta.ReasoningContent) + len(c.Delta.Reasoning)
			if overLimit(streamed) {
				return finish(), fmt.Errorf("response exceeded %d bytes; stopping", maxResponseBytes)
			}
			if r := c.Delta.ReasoningContent + c.Delta.Reasoning; r != "" {
				thinking.WriteString(r)
				out <- Event{Type: EventThinkingDelta, Text: r}
			}
			if c.Delta.Content != "" {
				text.WriteString(c.Delta.Content)
				out <- Event{Type: EventTextDelta, Text: c.Delta.Content}
			}
			for _, tc := range c.Delta.ToolCalls {
				idx := 0
				if tc.Index != nil {
					idx = *tc.Index
				}
				b, ok := toolCalls[idx]
				if !ok {
					// idx comes from the server, so the map is bounded here.
					if len(toolCalls) >= maxStreamBlocks {
						return finish(), fmt.Errorf("response opened more than %d tool calls; stopping", maxStreamBlocks)
					}
					b = &tcBuild{}
					toolCalls[idx] = b
					order = append(order, idx)
				}
				if tc.ID != "" {
					b.id = tc.ID
				}
				if tc.Function.Name != "" {
					if b.name == "" {
						out <- Event{Type: EventToolUseStart, ToolID: tc.ID, ToolName: tc.Function.Name}
					}
					b.name = tc.Function.Name
				}
				streamed += len(tc.Function.Arguments) + len(tc.Function.Name)
				if overLimit(streamed) {
					return finish(), fmt.Errorf("response exceeded %d bytes; stopping", maxResponseBytes)
				}
				b.args.WriteString(tc.Function.Arguments)
			}
			if c.FinishReason != "" {
				stopReason = c.FinishReason
			}
		}
	}
	if ctx.Err() != nil {
		return finish(), ctx.Err()
	}
	if err := scanner.Err(); err != nil {
		return finish(), fmt.Errorf("%s: reading stream: %w", o.name, err)
	}
	if stopReason == "" {
		return finish(), &APIError{Provider: o.name, Status: 502, Message: "stream ended before completion"}
	}
	out <- Event{Type: EventUsage, Usage: usage}
	out <- Event{Type: EventDone, StopReason: stopReason}
	return finish(), nil
}

// ListModels implements Provider.
func (o *OpenAI) ListModels(ctx context.Context) ([]string, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", o.modelsURL(), nil)
	if err != nil {
		return nil, err
	}
	if err := o.setAuth(req); err != nil {
		return nil, fmt.Errorf("%s: %w", o.name, err)
	}
	resp, err := o.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", o.name, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("%s: HTTP %d: %s", o.name, resp.StatusCode, apiErrorText(data))
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
