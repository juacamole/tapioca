package acp

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"tapioca/internal/config"
)

func testConfig(t *testing.T) *config.Config {
	t.Helper()
	t.Setenv("TAPIOCA_DATA_DIR", t.TempDir())
	t.Setenv("TAPIOCA_CONFIG_DIR", t.TempDir())
	cfg := config.Default()
	cfg.DefaultProvider = "ollama"
	cfg.DefaultModel = "test-model"
	cfg.PermissionMode = "bypass"
	cfg.MCP = nil
	return cfg
}

// fakeOllama answers /api/chat with a streamed reply, so the ACP layer is
// exercised over the real provider code path rather than a stubbed interface.
// With toolCall set, the first turn asks for a tool and the second answers.
type fakeOllama struct {
	mu       sync.Mutex
	prompt   string
	turns    int
	toolCall string // tool name to request on the first turn
	toolArgs string
}

func (f *fakeOllama) lastPrompt() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.prompt
}

// stubProvider points the config's ollama provider at a local fake.
func stubProvider(t *testing.T, cfg *config.Config) *fakeOllama {
	t.Helper()
	f := &fakeOllama{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Messages []struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"messages"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.mu.Lock()
		for _, m := range body.Messages {
			if m.Role == "user" {
				f.prompt = m.Content
			}
		}
		f.turns++
		turn, wantTool := f.turns, f.toolCall
		f.mu.Unlock()

		w.Header().Set("Content-Type", "application/x-ndjson")
		flush, _ := w.(http.Flusher)

		if wantTool != "" && turn == 1 {
			fmt.Fprintf(w, `{"message":{"role":"assistant","content":"","tool_calls":`+
				`[{"function":{"name":%s,"arguments":%s}}]},"done":true,"done_reason":"stop",`+
				`"prompt_eval_count":5,"eval_count":2}`+"\n", quote(wantTool), f.toolArgs)
			return
		}
		for _, chunk := range []string{"hi ", "there"} {
			fmt.Fprintf(w, `{"message":{"role":"assistant","content":%s},"done":false}`+"\n",
				quote(chunk))
			if flush != nil {
				flush.Flush()
			}
		}
		fmt.Fprint(w, `{"message":{"role":"assistant","content":""},"done":true,"done_reason":"stop",`+
			`"prompt_eval_count":5,"eval_count":2}`+"\n")
	}))
	t.Cleanup(srv.Close)

	p := cfg.Providers["ollama"]
	p.BaseURL = srv.URL
	cfg.Providers = map[string]config.ProviderConfig{"ollama": p}
	return f
}

func quote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}
