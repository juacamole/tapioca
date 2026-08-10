package provider

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"tapioca/internal/config"
)

// capture records how a provider actually talks to its endpoint.
type capture struct {
	mu     sync.Mutex
	path   string
	query  string
	header http.Header
	body   string
}

func (c *capture) server(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		c.mu.Lock()
		c.path, c.query, c.header, c.body = r.URL.Path, r.URL.RawQuery, r.Header.Clone(), string(body)
		c.mu.Unlock()

		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"},\"finish_reason\":null}]}\n\n")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	t.Cleanup(srv.Close)
	return srv
}

func drain(t *testing.T, p Provider, model string) {
	t.Helper()
	events := make(chan Event, 64)
	go func() {
		for range events {
		}
	}()
	if _, err := p.Stream(context.Background(), Request{Model: model, MaxTokens: 16,
		Messages: []Message{TextMessage("user", "hi")}}, events); err != nil {
		t.Fatalf("stream failed: %v", err)
	}
}

// Azure is the reason the plain openai type does not work for it: the model is
// a deployment in the path, the version is a query parameter, and the key goes
// in its own header rather than Authorization.
func TestAzureRequestShape(t *testing.T) {
	c := &capture{}
	srv := c.server(t)
	t.Setenv("AZURE_OPENAI_API_KEY", "azure-secret")

	p, err := New("azure", config.ProviderConfig{Type: "azure", BaseURL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	drain(t, p, "my-deployment")

	c.mu.Lock()
	defer c.mu.Unlock()
	if want := "/openai/deployments/my-deployment/chat/completions"; c.path != want {
		t.Errorf("path = %q, want %q", c.path, want)
	}
	if !strings.Contains(c.query, "api-version=") {
		t.Errorf("no api-version in query: %q", c.query)
	}
	if got := c.header.Get("api-key"); got != "azure-secret" {
		t.Errorf("api-key header = %q", got)
	}
	if got := c.header.Get("Authorization"); got != "" {
		t.Errorf("azure must not send Authorization, got %q", got)
	}
}

func TestAzureNeedsABaseURL(t *testing.T) {
	if _, err := New("azure", config.ProviderConfig{Type: "azure"}); err == nil {
		t.Fatal("azure without base_url should fail with an explanation")
	} else if !strings.Contains(err.Error(), "base_url") {
		t.Errorf("unhelpful error: %v", err)
	}
}

// Gemini's compatibility endpoint already ends in /openai, so the usual /v1
// prefix would 404.
func TestGeminiRequestShape(t *testing.T) {
	c := &capture{}
	srv := c.server(t)
	t.Setenv("GEMINI_API_KEY", "gem-secret")

	p, err := New("gemini", config.ProviderConfig{Type: "gemini", BaseURL: srv.URL + "/v1beta/openai"})
	if err != nil {
		t.Fatal(err)
	}
	drain(t, p, "gemini-2.0-flash")

	c.mu.Lock()
	defer c.mu.Unlock()
	if want := "/v1beta/openai/chat/completions"; c.path != want {
		t.Errorf("path = %q, want %q", c.path, want)
	}
	if got := c.header.Get("Authorization"); got != "Bearer gem-secret" {
		t.Errorf("Authorization = %q", got)
	}
	if !strings.Contains(c.body, `"model":"gemini-2.0-flash"`) {
		t.Errorf("model not sent in the body: %s", c.body)
	}
}

func TestGeminiDefaultsToGooglesEndpoint(t *testing.T) {
	p := NewGemini("gemini", config.ProviderConfig{})
	if got := p.chatURL("m"); got != geminiBase+"/chat/completions" {
		t.Errorf("default chat URL = %q", got)
	}
}

// Plain OpenAI keeps its existing shape.
func TestOpenAIShapeUnchanged(t *testing.T) {
	c := &capture{}
	srv := c.server(t)
	p, err := New("openai", config.ProviderConfig{Type: "openai", BaseURL: srv.URL, APIKey: "sk-x"})
	if err != nil {
		t.Fatal(err)
	}
	drain(t, p, "gpt-4o")

	c.mu.Lock()
	defer c.mu.Unlock()
	if c.path != "/v1/chat/completions" {
		t.Errorf("path = %q", c.path)
	}
	if got := c.header.Get("Authorization"); got != "Bearer sk-x" {
		t.Errorf("Authorization = %q", got)
	}
}

// /effort must reach the wire, and reasoning requests must not carry
// max_tokens or temperature.
func TestThinkingSendsReasoningEffort(t *testing.T) {
	c := &capture{}
	srv := c.server(t)
	p, err := New("openai", config.ProviderConfig{Type: "openai", BaseURL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	events := make(chan Event, 64)
	go func() {
		for range events {
		}
	}()
	_, err = p.Stream(context.Background(), Request{
		Model: "o4-mini", MaxTokens: 500, Temperature: 0.7,
		Thinking: true, ThinkingBudget: 8192,
		Messages: []Message{TextMessage("user", "hi")},
	}, events)
	if err != nil {
		t.Fatal(err)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if !strings.Contains(c.body, `"reasoning_effort":"high"`) {
		t.Errorf("reasoning_effort missing: %s", c.body)
	}
	if !strings.Contains(c.body, `"max_completion_tokens":500`) {
		t.Errorf("max_completion_tokens missing: %s", c.body)
	}
	if strings.Contains(c.body, `"max_tokens"`) || strings.Contains(c.body, `"temperature"`) {
		t.Errorf("reasoning request still carries max_tokens/temperature: %s", c.body)
	}
}
