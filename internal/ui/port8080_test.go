package ui

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"tapioca/internal/config"
)

// Nothing is contacted that the user did not configure. llama-server's port is
// 8080 — the one a Spring Boot app, a Tomcat or any dev server takes first —
// so looking for one nobody asked for means knocking on a stranger's door and
// putting whatever it says on screen.
func TestNoProviderIsContactedUntilItIsConfigured(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		fmt.Fprint(w, `{"data":[{"id":"user-1"}]}`)
	}))
	defer srv.Close()

	cfg := config.Default()
	cfg.Providers = map[string]config.ProviderConfig{} // nothing set up at all
	msg := probeConnections(cfg)().(connStatusMsg)

	if n := hits.Load(); n != 0 {
		t.Errorf("an unconfigured provider was contacted %d time(s)", n)
	}
	for _, e := range msg.entries {
		if e.external == nil && e.kind.Type == "llamacpp" && e.state != connUnset {
			t.Errorf("llamacpp reported as %v without being configured: %q", e.state, e.detail)
		}
	}
}

// Configured, but the address holds something else. A model list cannot tell
// them apart — {"data":[{"id":…}]} is an ordinary REST collection — so the
// server is asked for something only llama.cpp serves.
func TestConfiguredLlamaCppAtTheWrongAddressIsNotReady(t *testing.T) {
	cases := []struct {
		name    string
		handler http.HandlerFunc
	}{
		{"spring boot whitelabel 404", func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprint(w, `{"status":404,"error":"Not Found"}`)
		}},
		{"a rest api with data[].id", func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprint(w, `{"data":[{"id":"user-1"},{"id":"user-2"}]}`)
		}},
		{"props that is not llama.cpp's", func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/props" {
				fmt.Fprint(w, `{"default_generation_settings":{"temperature":0.7}}`)
				return
			}
			fmt.Fprint(w, `{"data":[{"id":"m"}]}`)
		}},
	}
	for _, c := range cases {
		srv := httptest.NewServer(c.handler)
		cfg := config.Default()
		cfg.Providers = map[string]config.ProviderConfig{
			"llamacpp": {Type: "llamacpp", BaseURL: srv.URL},
		}
		msg := probeConnections(cfg)().(connStatusMsg)
		for _, e := range msg.entries {
			if e.external == nil && e.kind.Type == "llamacpp" && e.state == connReady {
				t.Errorf("%s: reported ready (%q)", c.name, e.detail)
			}
		}
		srv.Close()
	}
}

// The control: a real llama-server, configured, is ready and lists its models.
func TestConfiguredLlamaCppIsReady(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/props" {
			fmt.Fprint(w, `{"default_generation_settings":{"n_ctx":8192}}`)
			return
		}
		fmt.Fprint(w, `{"data":[{"id":"/m/qwen3.gguf"}]}`)
	}))
	defer srv.Close()

	cfg := config.Default()
	cfg.Providers = map[string]config.ProviderConfig{
		"llamacpp": {Type: "llamacpp", BaseURL: srv.URL},
	}
	msg := probeConnections(cfg)().(connStatusMsg)
	for _, e := range msg.entries {
		if e.external == nil && e.kind.Type == "llamacpp" {
			if e.state != connReady || !strings.Contains(e.detail, "1 model") {
				t.Fatalf("a real llama-server was not ready: state=%v detail=%q", e.state, e.detail)
			}
			return
		}
	}
	t.Fatal("no llamacpp entry in the results")
}

// Every model carries the provider it actually came from, so two local servers
// holding the same weights stay tellable apart.
func TestEachModelKeepsItsOwnProvider(t *testing.T) {
	ollama := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"models":[{"name":"qwen3.8-27b-q5:latest"}]}`)
	}))
	defer ollama.Close()
	llama := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/props" {
			fmt.Fprint(w, `{"default_generation_settings":{"n_ctx":8192}}`)
			return
		}
		fmt.Fprint(w, `{"data":[{"id":"qwen3.8-27b-q5"}]}`)
	}))
	defer llama.Close()

	cfg := config.Default()
	cfg.Providers = map[string]config.ProviderConfig{
		"ollama":   {Type: "ollama", BaseURL: ollama.URL},
		"llamacpp": {Type: "llamacpp", BaseURL: llama.URL},
	}
	m := &App{cfg: cfg, w: 100, h: 30}
	got := map[string]string{}
	for _, it := range m.loadModelsCmd()().(modelsLoadedMsg).items {
		got[it.label] = it.desc
	}
	if got["qwen3.8-27b-q5"] != "llamacpp" {
		t.Errorf("llama.cpp's model was labelled %q", got["qwen3.8-27b-q5"])
	}
	if got["qwen3.8-27b-q5:latest"] != "ollama" {
		t.Errorf("ollama's model was labelled %q", got["qwen3.8-27b-q5:latest"])
	}
}

// Shipping a llamacpp entry would ask whatever holds 8080 for its models every
// time /model opens, and show its 404, for everyone not running llama.cpp.
func TestDefaultConfigDoesNotClaimPort8080(t *testing.T) {
	for name, pc := range config.Default().Providers {
		if strings.Contains(pc.BaseURL, ":8080") {
			t.Errorf("default config ships provider %q pointing at 8080: %s", name, pc.BaseURL)
		}
	}
}
