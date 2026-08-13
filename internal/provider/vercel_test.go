package provider

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"tapioca/internal/config"
)

// The gateway takes a plain bearer token, and its model ids are namespaced by
// vendor — the thing the rest of the app has to keep working with.
func TestVercelSendsABearerAndParsesNamespacedIDs(t *testing.T) {
	var got http.Header
	var path string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, path = r.Header.Clone(), r.URL.Path
		w.Write([]byte(`{"data":[{"id":"anthropic/claude-opus-5"},{"id":"openai/gpt-5.6-sol"}]}`))
	}))
	defer srv.Close()

	p := NewVercel("vercel", config.ProviderConfig{Type: "vercel", BaseURL: srv.URL, APIKey: "vk-test"})
	models, err := p.ListModels(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	if got.Get("Authorization") != "Bearer vk-test" {
		t.Errorf("Authorization = %q, want %q", got.Get("Authorization"), "Bearer vk-test")
	}
	if path != "/v1/models" {
		t.Errorf("requested %q, want /v1/models", path)
	}
	if len(models) != 2 || models[0] != "anthropic/claude-opus-5" {
		t.Errorf("models = %v, want the namespaced ids intact", models)
	}
}

// The address is the point of a first-class entry: nobody should have to know
// it, so the default has to be the real gateway and has to survive the /v1 trim.
func TestVercelDefaultsToTheGateway(t *testing.T) {
	p := NewVercel("vercel", config.ProviderConfig{Type: "vercel", APIKey: "k"})
	if got, want := p.chatURL("anthropic/claude-opus-5"), "https://ai-gateway.vercel.sh/v1/chat/completions"; got != want {
		t.Errorf("chatURL = %q, want %q", got, want)
	}
}

// Vercel's documented variable, so a key already exported is not asked for and
// stored a second time.
func TestVercelReadsTheDocumentedEnvVar(t *testing.T) {
	t.Setenv(vercelKeyEnv, "from-the-environment")
	p := NewVercel("vercel", config.ProviderConfig{Type: "vercel"})
	if p.apiKey != "from-the-environment" {
		t.Errorf("apiKey = %q, want the value from %s", p.apiKey, vercelKeyEnv)
	}
}

// Attribution is Tapioca naming itself, not the user. If either header ever
// carries a prompt, a model, or a key, that is a leak to a third party.
func TestVercelAttributionCarriesNothingPrivate(t *testing.T) {
	var got http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		w.Write([]byte(`{"data":[]}`))
	}))
	defer srv.Close()

	p := NewVercel("vercel", config.ProviderConfig{Type: "vercel", BaseURL: srv.URL, APIKey: "vk-secret"})
	if _, err := p.ListModels(context.Background()); err != nil {
		t.Fatal(err)
	}

	if got.Get("x-title") != "Tapioca" {
		t.Errorf("x-title = %q, want Tapioca", got.Get("x-title"))
	}
	for _, h := range []string{"http-referer", "x-title"} {
		if strings.Contains(got.Get(h), "vk-secret") {
			t.Errorf("%s carries the API key: %q", h, got.Get(h))
		}
	}
}

// A config header must be able to win, or a fork cannot stop announcing itself
// as this project — and it must still not be able to displace the credential.
func TestVercelConfigHeadersWinButCannotReplaceTheKey(t *testing.T) {
	var got http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		w.Write([]byte(`{"data":[]}`))
	}))
	defer srv.Close()

	p := NewVercel("vercel", config.ProviderConfig{
		Type: "vercel", BaseURL: srv.URL, APIKey: "vk-real",
		Headers: map[string]string{"x-title": "MyFork"},
	})
	if _, err := p.ListModels(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got.Get("x-title") != "MyFork" {
		t.Errorf("x-title = %q, want the configured MyFork", got.Get("x-title"))
	}

	hijack := NewVercel("vercel", config.ProviderConfig{
		Type: "vercel", BaseURL: srv.URL, APIKey: "vk-real",
		Headers: map[string]string{"Authorization": "Bearer someone-elses"},
	})
	if _, err := hijack.ListModels(context.Background()); err == nil {
		t.Error("a config header replaced the credential instead of being rejected")
	}
}

// With no key at all the request must go out bare, so the gateway answers with
// its own 401 rather than the app inventing a malformed token.
func TestVercelWithoutAKeySendsNoAuthorization(t *testing.T) {
	var got http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		w.Write([]byte(`{"data":[]}`))
	}))
	defer srv.Close()

	p := NewVercel("vercel", config.ProviderConfig{Type: "vercel", BaseURL: srv.URL})
	if _, err := p.ListModels(context.Background()); err != nil {
		t.Fatal(err)
	}
	if v := got.Get("Authorization"); v != "" {
		t.Errorf("Authorization = %q, want it absent", v)
	}
}
