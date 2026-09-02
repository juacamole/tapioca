package provider

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"tapioca/internal/config"
)

// llamaServer stands in for a llama-server, answering /props and /v1/models
// with whatever this test needs.
func llamaServer(t *testing.T, props, models string) *OpenAI {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/props":
			_, _ = w.Write([]byte(props))
		case "/v1/models":
			_, _ = w.Write([]byte(models))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return NewLlamaCpp("llamacpp", config.ProviderConfig{Type: "llamacpp", BaseURL: srv.URL})
}

// A router reports n_ctx 0 at /props and keeps reporting 0 even once a model is
// loaded, because the number belongs to the instance behind it. This is the
// shape a real one served (build b9925).
const routerPropsCtx = `{"role":"router","model_path":"none",` +
	`"default_generation_settings":{"params":null,"n_ctx":0},"build_info":"b9925-ed8c261"}`

// Two models with different windows on one server — 64k and 256k — which is why
// nothing provider-wide can stand in for a per-model answer. The loaded one
// carries meta; the unloaded one has only its launch arguments.
const routerModels = `{"data":[
 {"id":"small","aliases":[],"status":{"value":"unloaded","args":["llama-server","--alias","small","--ctx-size","65536"]}},
 {"id":"big","aliases":["biggie"],"status":{"value":"sleeping","args":["llama-server","--alias","big","--ctx-size","262144"]},
  "meta":{"n_ctx":262144,"n_ctx_train":262144}},
 {"id":"nosize","aliases":[],"status":{"value":"unloaded","args":["llama-server","--alias","nosize"]}}
]}`

// The bug: every model on a router fell back to the 8k default, so a session
// the server was holding 262k of was compacted at 8k.
func TestRouterReportsEachModelsOwnContext(t *testing.T) {
	o := llamaServer(t, routerPropsCtx, routerModels)
	for _, tc := range []struct {
		model string
		want  int
	}{
		{"small", 65536},
		{"big", 262144},
		{"biggie", 262144}, // by alias
	} {
		got, err := o.ContextLength(context.Background(), tc.model)
		if err != nil {
			t.Errorf("%s: %v", tc.model, err)
			continue
		}
		if got != tc.want {
			t.Errorf("%s: context = %d, want %d", tc.model, got, tc.want)
		}
	}
}

// The heart of it: one number cannot serve this server, so the answer has to
// depend on the argument.
func TestTwoModelsOnOneRouterDoNotShareAWindow(t *testing.T) {
	o := llamaServer(t, routerPropsCtx, routerModels)
	small, err := o.ContextLength(context.Background(), "small")
	if err != nil {
		t.Fatal(err)
	}
	big, err := o.ContextLength(context.Background(), "big")
	if err != nil {
		t.Fatal(err)
	}
	if small == big {
		t.Fatalf("both models report %d — the model argument is being ignored", small)
	}
}

// A server started on one model still answers from /props, which is the
// allocated context and the better source. This is the path that already
// worked and must keep working.
func TestASingleModelServerStillAnswersFromProps(t *testing.T) {
	o := llamaServer(t,
		`{"default_generation_settings":{"n_ctx":32768},"build_info":"b9925"}`,
		`{"data":[{"id":"whatever","status":{"args":["llama-server","--ctx-size","999"]}}]}`)

	got, err := o.ContextLength(context.Background(), "whatever")
	if err != nil {
		t.Fatal(err)
	}
	if got != 32768 {
		t.Errorf("context = %d, want the allocated 32768 from /props, not %d from the arguments", got, 999)
	}
}

// --ctx-size 0 means "take it from the model", which is not an answer. Neither
// is a model with no size at all. Reporting a made-up number is worse than
// falling back, because this number decides when a session is compacted.
func TestNoSizeIsRefusedRatherThanGuessed(t *testing.T) {
	o := llamaServer(t, routerPropsCtx, routerModels)
	if n, err := o.ContextLength(context.Background(), "nosize"); err == nil {
		t.Errorf("context = %d with no error, want a refusal", n)
	}
}

func TestZeroCtxSizeIsNotAnAnswer(t *testing.T) {
	o := llamaServer(t, routerPropsCtx,
		`{"data":[{"id":"m","status":{"args":["llama-server","--ctx-size","0"]}}]}`)
	if n, err := o.ContextLength(context.Background(), "m"); err == nil {
		t.Errorf("context = %d, want a refusal — 0 means take it from the model", n)
	}
}

// A model the server does not have must not inherit another model's window.
func TestAnUnknownModelDoesNotBorrowAWindow(t *testing.T) {
	o := llamaServer(t, routerPropsCtx, routerModels)
	if n, err := o.ContextLength(context.Background(), "absent"); err == nil {
		t.Errorf("context = %d for a model that is not on the server, want a refusal", n)
	}
}

// ListModels shortens a GGUF path to its basename for the picker, so the id
// that comes back to be asked about is not the id the server printed.
func TestAGGUFPathIsMatchedByItsBasename(t *testing.T) {
	o := llamaServer(t, routerPropsCtx,
		`{"data":[{"id":"/home/k/models/Qwen3-27B-UD-Q4.gguf","status":{"args":["llama-server","--ctx-size","131072"]}}]}`)

	models, err := o.ListModels(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(models) != 1 {
		t.Fatalf("models = %v", models)
	}
	got, err := o.ContextLength(context.Background(), models[0])
	if err != nil {
		t.Fatalf("%s: %v", models[0], err)
	}
	if got != 131072 {
		t.Errorf("context = %d, want 131072", got)
	}
}

func TestCtxSizeArg(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		want int
	}{
		{"long form", []string{"llama-server", "--ctx-size", "4096"}, 4096},
		{"short form", []string{"llama-server", "-c", "8192"}, 8192},
		{"absent", []string{"llama-server", "--jinja"}, 0},
		{"zero means from the model", []string{"--ctx-size", "0"}, 0},
		{"negative", []string{"--ctx-size", "-1"}, 0},
		{"unparseable", []string{"--ctx-size", "lots"}, 0},
		{"nothing after it", []string{"--ctx-size"}, 0},
	} {
		if got := ctxSizeArg(tc.args); got != tc.want {
			t.Errorf("%s: ctxSizeArg(%v) = %d, want %d", tc.name, tc.args, got, tc.want)
		}
	}
}
