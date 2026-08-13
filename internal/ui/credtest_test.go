package ui

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"tapioca/internal/config"
	"tapioca/internal/provider"
)

// The credential test has to check the provider that will actually be saved.
// saveCredential starts from the existing config entry and overlays what was
// typed, so a field the form never asks about — a base_url pointing at a proxy,
// an api_version, an extra header — is part of the result. Testing a blank
// config instead means the screen reports "connected" about a provider that is
// not the one written to disk.
func TestTheCredentialTestUsesTheConfigThatWillBeSaved(t *testing.T) {
	var hit bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hit = true
		w.Write([]byte(`{"data":[{"id":"anthropic/claude-opus-5"}]}`))
	}))
	defer srv.Close()

	// A base_url override is the case that matters: the vercel form asks only
	// for a key, so the address can only come from the existing entry.
	existing := config.ProviderConfig{Type: "vercel", BaseURL: srv.URL}
	k, ok := provider.KindFor("vercel")
	if !ok {
		t.Fatal("no vercel catalog entry")
	}

	cmd := testProvider(k, existing, map[string]string{"api_key": "vk-test"})
	msg, _ := cmd().(credTestedMsg)

	if !hit {
		t.Error("the configured base_url was ignored — the test hit a different server than the one that will be saved")
	}
	if msg.err != nil {
		t.Errorf("test failed: %v", msg.err)
	}
	if msg.models != 1 {
		t.Errorf("models = %d, want 1", msg.models)
	}
}

// What was typed still has to win over what was there, or correcting a bad key
// re-tests the bad one and reports success.
func TestTypedValuesOverrideTheExistingConfig(t *testing.T) {
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("Authorization")
		w.Write([]byte(`{"data":[]}`))
	}))
	defer srv.Close()

	existing := config.ProviderConfig{Type: "vercel", BaseURL: srv.URL, APIKey: "vk-the-old-broken-one"}
	k, _ := provider.KindFor("vercel")

	cmd := testProvider(k, existing, map[string]string{"api_key": "vk-the-new-one"})
	cmd()

	if got != "Bearer vk-the-new-one" {
		t.Errorf("Authorization = %q, want the newly typed key", got)
	}
}
