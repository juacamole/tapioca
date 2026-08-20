package ui

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"tapioca/internal/config"
	"tapioca/internal/provider"
)

// detectAt is detectOne pointed at a stub instead of the real default address.
func detectAt(t *testing.T, base string) connEntry {
	t.Helper()
	k, _ := provider.KindFor("llamacpp")
	pc := config.ProviderConfig{Type: k.Type, BaseURL: base}
	p, err := provider.New(k.Type, pc)
	if err != nil {
		t.Fatal(err)
	}
	id, ok := p.(provider.Identifier)
	if !ok {
		t.Fatal("llamacpp cannot identify itself")
	}
	if err := id.Identify(t.Context()); err != nil {
		return connEntry{kind: k, state: connUnset, detail: "not configured"}
	}
	e := probeOne(k, k.Type, pc)
	e.detected = e.state == connReady
	return e
}

// 8080 is the port a Spring Boot app takes first. Whatever is there answers,
// and none of it makes the address a model server.
func TestSomethingElseOn8080IsNotAModelServer(t *testing.T) {
	cases := []struct {
		name    string
		handler http.HandlerFunc
	}{
		{"spring boot whitelabel 404", func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprint(w, `{"timestamp":"2026-08-20","status":404,"error":"Not Found"}`)
		}},
		{"html for everything", func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprint(w, `<!doctype html><html><body>hello</body></html>`)
		}},
		{"actuator health", func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprint(w, `{"status":"UP"}`)
		}},
		// The one that got through: an ordinary REST collection has the same
		// shape as an OpenAI model list.
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
		e := detectAt(t, srv.URL)
		if e.detected || e.state == connReady {
			t.Errorf("%s: offered as a model server (detail %q)", c.name, e.detail)
		}
		if e.state == connFailing {
			t.Errorf("%s: reported as a failure; it is simply not llama.cpp", c.name)
		}
		srv.Close()
	}
}

// The control: a real llama-server says what it is, and is found.
func TestARealLlamaServerIsStillFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/props" {
			fmt.Fprint(w, `{"default_generation_settings":{"n_ctx":8192},"model_path":"/m/qwen3.gguf"}`)
			return
		}
		fmt.Fprint(w, `{"data":[{"id":"/m/qwen3.gguf"}]}`)
	}))
	defer srv.Close()

	e := detectAt(t, srv.URL)
	if !e.detected || e.state != connReady {
		t.Fatalf("a real llama-server was not found: state=%v detail=%q", e.state, e.detail)
	}
	if !strings.Contains(e.detail, "1 model") {
		t.Errorf("detail = %q", e.detail)
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
