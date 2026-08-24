package provider

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"tapioca/internal/config"
)

func llamaAt(t *testing.T, props string) *OpenAI {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/props" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(props))
	}))
	t.Cleanup(srv.Close)
	return NewLlamaCpp("llamacpp", config.ProviderConfig{Type: "llamacpp", BaseURL: srv.URL})
}

// A llama-server in router mode fronts several models and loads them on
// demand, so at the moment it is asked it has allocated no context and reports
// n_ctx 0. This is the shape a real one served (build b9925). Requiring an
// allocated n_ctx to prove identity rejected it as "no n_ctx in /props" — a
// working server with five models on it, reported as the wrong address.
const routerProps = `{"role":"router","max_instances":1,"models_autoload":true,` +
	`"model_alias":"llama-server","model_path":"none",` +
	`"default_generation_settings":{"params":null,"n_ctx":0},` +
	`"build_info":"b9925-ed8c261","cors_proxy_enabled":false}`

func TestARouterModeServerIsIdentified(t *testing.T) {
	if err := llamaAt(t, routerProps).Identify(context.Background()); err != nil {
		t.Errorf("a real llama-server in router mode was not identified: %v", err)
	}
}

// A server started on a model still identifies, and that is still where the
// gauge's number comes from.
func TestALoadedServerIdentifiesAndReportsItsContext(t *testing.T) {
	o := llamaAt(t, `{"default_generation_settings":{"n_ctx":32768},"build_info":"b9925-ed8c261"}`)
	if err := o.Identify(context.Background()); err != nil {
		t.Fatalf("Identify: %v", err)
	}
	n, err := o.ContextLength(context.Background(), "")
	if err != nil {
		t.Fatalf("ContextLength: %v", err)
	}
	if n != 32768 {
		t.Errorf("n_ctx = %d, want 32768", n)
	}
}

// Identity and context size are two questions. A router can answer the first
// and not the second, and the gauge must be told that rather than handed a
// zero it would render as a full context.
func TestARouterReportsNoContextLength(t *testing.T) {
	if n, err := llamaAt(t, routerProps).ContextLength(context.Background(), ""); err == nil {
		t.Errorf("ContextLength = %d with no error, want a refusal — nothing is loaded to have a context", n)
	}
}

// The check this widening must not have given up: 8080 is the most contested
// port there is, and something else answering on it is not a model server.
func TestSomethingElseOn8080IsStillRefused(t *testing.T) {
	// A Spring Boot app: /props is not a route it has, so it 404s.
	if err := llamaAt(t, "").Identify(context.Background()); err == nil {
		t.Error("a server with no /props was identified as llama.cpp")
	}
	// And one that does answer /props with JSON of its own, holding none of
	// llama.cpp's fields — the case the widened check has to still catch.
	other := llamaAt(t, `{"status":"UP","groups":["liveness","readiness"]}`)
	err := other.Identify(context.Background())
	if err == nil {
		t.Fatal("an unrelated JSON /props was identified as llama.cpp")
	}
	if !strings.Contains(err.Error(), "nothing in it is llama.cpp's") {
		t.Errorf("refused for the wrong reason: %v", err)
	}
}
