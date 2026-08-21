package cli

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// Launch's own doc comment says why it exists: the ACP sessions are built in
// another package, so "a flag folded into a local variable in Main reached the
// TUI and nothing else". --permission-mode was carried across for exactly that
// reason. --model, --system-prompt and --append-system-prompt were not: they
// are applied at the bottom of Main, onto mgr.Agents, a manager the ACP branch
// returns long before anything creates.
//
// So `tapioca --acp --model anthropic:claude-opus-5` — which is how an editor
// is told which model to run — silently ran whatever default_model said, and
// with no default_model at all it refused every session with "no model
// configured" while the flag naming one sat unread.
type acpProbe struct {
	mu     sync.Mutex
	model  string
	system string
}

func (p *acpProbe) seen() (string, string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.model, p.system
}

// fakeOllamaProbe answers one turn with plain text and records the model and
// system prompt it was asked for.
func fakeOllamaProbe(t *testing.T) (string, *acpProbe) {
	t.Helper()
	p := &acpProbe{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Model    string `json:"model"`
			Messages []struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"messages"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		p.mu.Lock()
		p.model = body.Model
		for _, m := range body.Messages {
			if m.Role == "system" {
				p.system = m.Content
			}
		}
		p.mu.Unlock()
		w.Header().Set("Content-Type", "application/x-ndjson")
		fmt.Fprint(w, `{"message":{"role":"assistant","content":"done"},"done":true,`+
			`"done_reason":"stop","prompt_eval_count":5,"eval_count":2}`+"\n")
	}))
	t.Cleanup(srv.Close)
	return srv.URL, p
}

// runACPPrompt starts the binary in ACP mode with the given extra flags and
// drives one prompt through it.
func runACPPrompt(t *testing.T, bin string, extra ...string) *acpProbe {
	t.Helper()
	work := t.TempDir()
	base, probe := fakeOllamaProbe(t)

	cfgDir := t.TempDir()
	cfgPath := filepath.Join(cfgDir, "config.toml")
	body := "default_provider = \"ollama\"\ndefault_model = \"file-model\"\n" +
		"model_catalog = false\npermission_mode = \"bypass\"\n\n" +
		"[providers.ollama]\ntype = \"ollama\"\nbase_url = " + quoteTOML(base) + "\n"
	if err := os.WriteFile(cfgPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	args := append([]string{"--acp", "--settings", cfgPath}, extra...)
	cmd := exec.Command(bin, args...)
	cmd.Dir = work
	cmd.Env = append(os.Environ(),
		"TAPIOCA_CONFIG_DIR="+cfgDir,
		"TAPIOCA_DATA_DIR="+t.TempDir(),
		"HOME="+t.TempDir(),
	)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		stdin.Close()
		done := make(chan struct{})
		go func() { _ = cmd.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			_ = cmd.Process.Kill()
			<-done
		}
	}()

	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	id := 0
	call := func(method string, params any) json.RawMessage {
		t.Helper()
		id++
		req, _ := json.Marshal(map[string]any{
			"jsonrpc": "2.0", "id": id, "method": method, "params": params,
		})
		if _, err := stdin.Write(append(req, '\n')); err != nil {
			t.Fatalf("writing %s: %v", method, err)
		}
		for sc.Scan() {
			var msg struct {
				ID     json.Number     `json:"id"`
				Result json.RawMessage `json:"result"`
				Error  json.RawMessage `json:"error"`
				Method string          `json:"method"`
			}
			if json.Unmarshal(sc.Bytes(), &msg) != nil || msg.Method != "" {
				continue
			}
			if n, err := msg.ID.Int64(); err == nil && int(n) == id {
				if msg.Error != nil {
					t.Fatalf("%s failed: %s", method, msg.Error)
				}
				return msg.Result
			}
		}
		t.Fatalf("the server closed before replying to %s", method)
		return nil
	}

	call("initialize", map[string]any{
		"protocolVersion":    1,
		"clientCapabilities": map[string]any{},
		"clientInfo":         map[string]any{"name": "test-editor", "version": "1"},
	})
	var newSess struct {
		SessionID string `json:"sessionId"`
	}
	if err := json.Unmarshal(call("session/new", map[string]any{
		"cwd": work, "mcpServers": []any{},
	}), &newSess); err != nil {
		t.Fatal(err)
	}
	call("session/prompt", map[string]any{
		"sessionId": newSess.SessionID,
		"prompt":    []any{map[string]any{"type": "text", "text": "hello"}},
	})
	return probe
}

func TestACPHonoursTheModelFlag(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the binary")
	}
	bin := buildTapio(t)

	// The control: with no flag the file's model is what runs, so the harness
	// really does reach the provider and the assertion below is about the flag.
	if model, _ := runACPPrompt(t, bin).seen(); model != "file-model" {
		t.Skipf("the harness never reached the provider (model = %q); the flag test would be vacuous", model)
	}

	model, _ := runACPPrompt(t, bin, "--model", "flag-model").seen()
	if model != "flag-model" {
		t.Errorf("--acp --model flag-model ran %q: the launch flag never reached the ACP sessions", model)
	}
}

func TestACPHonoursTheSystemPromptFlags(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the binary")
	}
	bin := buildTapio(t)

	// The default prompt, which --system-prompt has to displace. The agent adds
	// the working directory and its tool notes underneath either one, so the
	// question is which text is in there and not what the whole string is.
	_, base := runACPPrompt(t, bin).seen()
	const marker = "pragmatic coding assistant"
	if !contains(base, marker) {
		t.Skipf("the default prompt is not what this build sends (%q); the flag test would be vacuous", trunc(base))
	}

	_, replaced := runACPPrompt(t, bin, "--system-prompt", "PROMPT-FROM-FLAG").seen()
	if !contains(replaced, "PROMPT-FROM-FLAG") || contains(replaced, marker) {
		t.Errorf("--acp --system-prompt did not reach the ACP sessions: system prompt is %q", trunc(replaced))
	}

	_, appended := runACPPrompt(t, bin, "--append-system-prompt", "APPENDED-BY-FLAG").seen()
	if !contains(appended, "APPENDED-BY-FLAG") {
		t.Errorf("--acp --append-system-prompt did not reach the ACP sessions: system prompt is %q", trunc(appended))
	}
}

func contains(hay, needle string) bool {
	return len(hay) >= len(needle) && (func() bool {
		for i := 0; i+len(needle) <= len(hay); i++ {
			if hay[i:i+len(needle)] == needle {
				return true
			}
		}
		return false
	})()
}

func trunc(s string) string {
	if len(s) > 200 {
		return s[:200] + "…"
	}
	return s
}
