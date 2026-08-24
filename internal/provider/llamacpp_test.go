package provider

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"tapioca/internal/config"
)

// llama-server has no credentials by default, so the request must go out
// clean — an empty Bearer header makes some proxies reject it.
func TestLlamaCppRequestShape(t *testing.T) {
	c := &capture{}
	srv := c.server(t)
	t.Setenv("LLAMA_API_KEY", "")

	p, err := New("llamacpp", config.ProviderConfig{Type: "llamacpp", BaseURL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	drain(t, p, "qwen3.gguf")

	c.mu.Lock()
	defer c.mu.Unlock()
	if c.path != "/v1/chat/completions" {
		t.Errorf("path = %q", c.path)
	}
	if got := c.header.Get("Authorization"); got != "" {
		t.Errorf("keyless server must not get Authorization, got %q", got)
	}
}

// A base URL pasted with its /v1 must not become /v1/v1, same as the plain
// openai type.
func TestLlamaCppTrimsVersionSuffix(t *testing.T) {
	p := NewLlamaCpp("l", config.ProviderConfig{BaseURL: "http://localhost:8080/v1"})
	if got := p.chatURL("m"); got != "http://localhost:8080/v1/chat/completions" {
		t.Errorf("chat URL = %q", got)
	}
}

func TestLlamaCppDefaultsToPort8080(t *testing.T) {
	t.Setenv("LLAMA_API_KEY", "")
	p := NewLlamaCpp("l", config.ProviderConfig{})
	if got := p.chatURL("m"); got != "http://localhost:8080/v1/chat/completions" {
		t.Errorf("default chat URL = %q", got)
	}
}

// Tapioca sends tool definitions on every request, and a llama-server started
// without --jinja refuses them all. The raw 500 names jinja but not the fix,
// so the error must.
func TestLlamaCppJinjaErrorNamesTheFix(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprint(w, `{"error":{"code":500,"message":"tools param requires --jinja flag","type":"server_error"}}`)
	}))
	defer srv.Close()
	t.Setenv("LLAMA_API_KEY", "")

	p, err := New("llamacpp", config.ProviderConfig{Type: "llamacpp", BaseURL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	events := make(chan Event, 64)
	go func() {
		for range events {
		}
	}()
	_, err = p.Stream(context.Background(), Request{Model: "m", MaxTokens: 16,
		Messages: []Message{TextMessage("user", "hi")}}, events)
	if err == nil {
		t.Fatal("a 500 must fail the request")
	}
	if !strings.Contains(err.Error(), "llama-server --jinja") {
		t.Errorf("error does not say how to fix it: %v", err)
	}
}

// The same status from a hosted provider must stay untouched — the hint is a
// llama.cpp diagnosis, not a generic one.
func TestJinjaHintOnlyForLlamaCpp(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprint(w, `{"error":{"message":"template error: jinja"}}`)
	}))
	defer srv.Close()

	p, err := New("openai", config.ProviderConfig{Type: "openai", BaseURL: srv.URL, APIKey: "k"})
	if err != nil {
		t.Fatal(err)
	}
	events := make(chan Event, 64)
	go func() {
		for range events {
		}
	}()
	_, err = p.Stream(context.Background(), Request{Model: "m", MaxTokens: 16,
		Messages: []Message{TextMessage("user", "hi")}}, events)
	if err == nil {
		t.Fatal("a 500 must fail the request")
	}
	if strings.Contains(err.Error(), "llama-server") {
		t.Errorf("hosted provider got the llama.cpp hint: %v", err)
	}
}

// The gauge must reflect what this server was started with, not the model's
// trained window — llama-server reports it on /props.
func TestLlamaCppContextLength(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/props" {
			http.NotFound(w, r)
			return
		}
		fmt.Fprint(w, `{"default_generation_settings":{"n_ctx":32768},"model_path":"/m/qwen3.gguf"}`)
	}))
	defer srv.Close()
	t.Setenv("LLAMA_API_KEY", "")

	p := NewLlamaCpp("l", config.ProviderConfig{BaseURL: srv.URL})
	n, err := p.ContextLength(context.Background(), "qwen3.gguf")
	if err != nil {
		t.Fatal(err)
	}
	if n != 32768 {
		t.Errorf("n_ctx = %d, want 32768", n)
	}
}

func TestContextLengthOnlyForLlamaCpp(t *testing.T) {
	p := NewOpenAI("o", config.ProviderConfig{APIKey: "k"})
	if _, err := p.ContextLength(context.Background(), "gpt-4o"); err == nil {
		t.Fatal("hosted flavours have no /props to probe")
	}
}

// A single-model server reports the GGUF's full path as the model id and
// ignores the field on requests; the path only clutters the picker. Router
// names and --alias values carry no slash and must pass through.
func TestLlamaCppListModelsBasenamesPaths(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"object":"list","data":[{"id":"/home/kuan/models/Qwen3.6-27B-UD-Q4_K_XL.gguf"},{"id":"my-alias"}]}`)
	}))
	defer srv.Close()
	t.Setenv("LLAMA_API_KEY", "")

	p := NewLlamaCpp("l", config.ProviderConfig{BaseURL: srv.URL})
	models, err := p.ListModels(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"Qwen3.6-27B-UD-Q4_K_XL", "my-alias"}
	if len(models) != 2 || models[0] != want[0] || models[1] != want[1] {
		t.Errorf("models = %v, want %v", models, want)
	}
}

// The aliases New accepts must resolve in the catalog too, or a working
// config is reported as an unknown provider.
func TestLlamaCppKindAliases(t *testing.T) {
	for _, typ := range []string{"llamacpp", "llama.cpp", "llama-cpp"} {
		k, ok := KindFor(typ)
		if !ok || k.Type != "llamacpp" {
			t.Errorf("KindFor(%q) = %+v, %v", typ, k, ok)
		}
	}
}

func TestIsLocal(t *testing.T) {
	for _, typ := range []string{"", "ollama", "llamacpp", "llama.cpp", "llama-cpp"} {
		if !IsLocal(typ) {
			t.Errorf("IsLocal(%q) = false", typ)
		}
	}
	for _, typ := range []string{"openai", "anthropic", "custom", "vercel"} {
		if IsLocal(typ) {
			t.Errorf("IsLocal(%q) = true", typ)
		}
	}
}
