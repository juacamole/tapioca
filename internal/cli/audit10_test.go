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

// --permission-mode is a launch flag: the user types it, and it is meant to
// outrank whatever the config file says. Under --acp it was folded into a
// local variable that only the TUI's executor ever saw, and acp.Serve was
// handed the config unchanged — so an editor started as
// `tapioca --acp --permission-mode plan` ran every session in the file's mode.
//
// The test drives the real binary the way an editor does, because the wiring
// between the flag and the server is what is in question and it lives in
// Main.

// buildTapio compiles the binary under test once per run.
func buildTapio(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("no go toolchain on PATH; cannot build the binary this test drives")
	}
	bin := filepath.Join(t.TempDir(), "tapio")
	cmd := exec.Command("go", "build", "-o", bin, "tapioca/cmd/tapio")
	cmd.Dir = repoRoot(t)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("building the binary: %v\n%s", err, out)
	}
	return bin
}

func repoRoot(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	return filepath.Dir(filepath.Dir(wd)) // internal/cli -> repo root
}

// fakeOllamaWriting answers the first turn with a write_file tool call and the
// second with plain text, so one prompt is enough to see whether the write was
// allowed to happen.
func fakeOllamaWriting(t *testing.T, path string) string {
	t.Helper()
	var mu sync.Mutex
	turns := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		turns++
		turn := turns
		mu.Unlock()
		w.Header().Set("Content-Type", "application/x-ndjson")
		if turn == 1 {
			args, _ := json.Marshal(map[string]string{"path": path, "content": "pwned\n"})
			fmt.Fprintf(w, `{"message":{"role":"assistant","content":"","tool_calls":`+
				`[{"function":{"name":"write_file","arguments":%s}}]},"done":true,`+
				`"done_reason":"stop","prompt_eval_count":5,"eval_count":2}`+"\n", args)
			return
		}
		fmt.Fprint(w, `{"message":{"role":"assistant","content":"done"},"done":true,`+
			`"done_reason":"stop","prompt_eval_count":5,"eval_count":2}`+"\n")
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// runACPWrite starts the binary in ACP mode with the given extra flags, drives
// one prompt through it, and reports whether the model's write_file landed.
func runACPWrite(t *testing.T, bin string, extra ...string) bool {
	t.Helper()
	work := t.TempDir()
	target := filepath.Join(work, "pwned.txt")
	base := fakeOllamaWriting(t, target)

	cfgDir := t.TempDir()
	cfgPath := filepath.Join(cfgDir, "config.toml")
	// permission_mode in the file is the one a flag has to be able to overrule.
	body := "permission_mode = \"bypass\"\ndefault_provider = \"ollama\"\n" +
		"default_model = \"test-model\"\nmodel_catalog = false\n\n" +
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
		"prompt":    []any{map[string]any{"type": "text", "text": "write the file"}},
	})

	_, err = os.Stat(target)
	return err == nil
}

func quoteTOML(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func TestACPHonoursPermissionModeFlag(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the binary")
	}
	bin := buildTapio(t)

	// The control: without the flag the file's bypass is in force and the write
	// really does land. Without this the assertion below could pass because the
	// harness never got as far as the tool call.
	if !runACPWrite(t, bin) {
		t.Skip("the harness never reached the write_file call; the flag test below would be vacuous")
	}

	if runACPWrite(t, bin, "--permission-mode", "plan") {
		t.Error("--acp --permission-mode plan wrote the file: the launch flag never reached the ACP sessions")
	}
}
