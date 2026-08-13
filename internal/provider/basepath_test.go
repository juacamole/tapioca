package provider

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"tapioca/internal/config"
)

// Every gateway publishes its address with the version segment already on it —
// https://ai-gateway.vercel.sh/v1, https://openrouter.ai/api/v1,
// https://api.groq.com/openai/v1 — and the OpenAI client appends /v1 itself. So
// a base URL pasted from the provider's own documentation asked for
// /v1/v1/models and got a 404 that named nothing.
func TestBaseURLKeepsItsVersionSegment(t *testing.T) {
	for _, base := range []string{"", "/v1", "/v1/"} {
		var path string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			path = r.URL.Path
			w.Write([]byte(`{"data":[{"id":"anthropic/claude-opus-5"}]}`))
		}))

		p, err := NewCustom("c", config.ProviderConfig{
			Type: "custom", BaseURL: srv.URL + base, APIKey: "k",
		})
		if err != nil {
			t.Fatalf("base %q: %v", base, err)
		}
		if _, err := p.ListModels(context.Background()); err != nil {
			t.Fatalf("base %q: %v", base, err)
		}
		srv.Close()

		if path != "/v1/models" {
			t.Errorf("base_url ending %q requested %q, want /v1/models", base, path)
		}
	}
}

// The chat endpoint is what a prompt actually goes to, so it has to survive the
// same paste.
func TestChatURLKeepsItsVersionSegment(t *testing.T) {
	p, err := NewCustom("c", config.ProviderConfig{
		Type: "custom", BaseURL: "https://api.example.com/v1", APIKey: "k",
	})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := p.chatURL("m"), "https://api.example.com/v1/chat/completions"; got != want {
		t.Errorf("chatURL = %q, want %q", got, want)
	}
}
