package tools

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"tapioca/internal/secretenv"
)

// Tapioca's config holds provider keys and MCP bearer tokens; its data dir
// holds every past conversation. Neither counted as sensitive, so both read
// with no prompt in manual mode.
func TestOwnConfigAndSessionsAreSensitive(t *testing.T) {
	e := execIn(t, ModeManual)
	cfgDir := t.TempDir()
	dataDir := t.TempDir()
	t.Setenv("TAPIOCA_CONFIG_DIR", cfgDir)
	t.Setenv("TAPIOCA_DATA_DIR", dataDir)

	cfg := filepath.Join(cfgDir, "config.toml")
	if err := os.WriteFile(cfg, []byte(`api_key = "sk-CANARY"`), 0o600); err != nil {
		t.Fatal(err)
	}
	sess := filepath.Join(dataDir, "sessions", "20260101-000000.json")
	if err := os.MkdirAll(filepath.Dir(sess), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sess, []byte(`{"id":"x","agents":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}

	for _, path := range []string{cfg, sess} {
		if !e.sensitivePath(path) {
			t.Errorf("%s did not read as sensitive", path)
		}
		var asked []string
		out, isErr, err := e.Call(context.Background(), "read_file",
			args(t, map[string]string{"path": path}), asker(Decision{Allow: false}, &asked))
		if err != nil {
			t.Fatal(err)
		}
		if len(asked) == 0 || !isErr {
			t.Errorf("%s read without a prompt: %q", path, out)
		}
	}
}

func TestExtendedSensitiveNames(t *testing.T) {
	e := execIn(t, ModeManual)
	home := t.TempDir()
	t.Setenv("HOME", home)
	for _, name := range []string{".npmrc", ".pypirc", ".bash_history", ".zsh_history"} {
		p := filepath.Join(home, name)
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		if !e.sensitivePath(p) {
			t.Errorf("%s did not read as sensitive", name)
		}
	}
	// A source file with an innocuous name in the worktree still must not.
	src := filepath.Join(e.Cwd(), "main.go")
	if err := os.WriteFile(src, []byte("package main"), 0o600); err != nil {
		t.Fatal(err)
	}
	if e.sensitivePath(src) {
		t.Error("an ordinary source file now prompts, which would make the gate useless")
	}
}

// Tapioca reads GOOGLE_ACCESS_TOKEN itself for Vertex, so it is a live bearer
// token sitting in the environment of every tool call.
func TestScrubCoversTokensTapiocaItselfUses(t *testing.T) {
	secretenv.SetExtra(nil)
	for _, name := range []string{
		"GOOGLE_ACCESS_TOKEN", "GOOGLE_APPLICATION_CREDENTIALS",
		"AWS_ACCESS_KEY_ID", "ANTHROPIC_AUTH_TOKEN", "GITHUB_TOKEN", "GH_TOKEN",
	} {
		t.Setenv(name, "CANARY-"+name)
	}
	t.Setenv("PATH", os.Getenv("PATH"))
	for _, kv := range secretenv.Scrubbed() {
		if len(kv) > 7 && kv[:7] == "CANARY-" {
			t.Errorf("unscrubbed: %s", kv)
		}
		for _, name := range []string{"GOOGLE_ACCESS_TOKEN", "AWS_ACCESS_KEY_ID", "GITHUB_TOKEN"} {
			if kv == name+"=CANARY-"+name {
				t.Errorf("%s reached a subprocess", name)
			}
		}
	}
	// PATH and the rest of the environment must survive, or tools break.
	found := false
	for _, kv := range secretenv.Scrubbed() {
		if len(kv) > 5 && kv[:5] == "PATH=" {
			found = true
		}
	}
	if !found {
		t.Fatal("PATH was scrubbed too")
	}
}

// The cloud metadata endpoint was reachable by asking for it directly: only
// redirect targets were screened. Bypass mode, so nothing else can stop it.
func TestWebFetchRefusesLinkLocalAddresses(t *testing.T) {
	e := execIn(t, ModeBypass)
	for _, url := range []string{
		"http://169.254.169.254/latest/meta-data/iam/security-credentials/",
		"http://169.254.169.254/computeMetadata/v1/",
		"http://[fe80::1]/",
	} {
		out, isErr, err := e.Call(context.Background(), "web_fetch",
			args(t, map[string]string{"url": url}), asker(Decision{Allow: true}, new([]string)))
		if err != nil {
			t.Fatal(err)
		}
		if !isErr {
			t.Errorf("%s was fetched: %q", url, out)
		}
	}
}

// Fetching your own dev server is a real thing to want, and it already goes
// through the per-host prompt. Over-blocking here would be a regression, not
// a fix — the existing redirect tests fetch a loopback httptest server.
func TestWebFetchStillAllowsLoopback(t *testing.T) {
	if linkLocalHost("127.0.0.1") || linkLocalHost("localhost") || linkLocalHost("10.0.0.1") {
		t.Fatal("an ordinary local address was classed as link-local")
	}
	// The redirect guard stays stricter: the user approved a different host.
	if !internalHost("127.0.0.1") {
		t.Fatal("the redirect guard stopped treating loopback as internal")
	}
}

// read_file needs no approval in any mode and had no size cap, so os.ReadFile
// on a character device grew a buffer until the process died — taking the TUI
// and everything since the last save with it.
func TestReadFileIsCapped(t *testing.T) {
	e := execIn(t, ModeBypass)
	if _, err := os.Stat("/dev/zero"); err != nil {
		t.Skip("no /dev/zero here")
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		out, _, _ := e.Call(context.Background(), "read_file",
			args(t, map[string]string{"path": "/dev/zero"}), asker(Decision{Allow: true}, new([]string)))
		if len(out) > 32<<20 {
			t.Errorf("read %d bytes from /dev/zero", len(out))
		}
	}()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("reading /dev/zero never returned")
	}
}

// A real file must still come back whole.
func TestReadFileStillReadsOrdinaryFiles(t *testing.T) {
	e := execIn(t, ModeBypass)
	p := filepath.Join(e.Cwd(), "hello.txt")
	if err := os.WriteFile(p, []byte("line one\nline two\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	out, isErr, err := e.Call(context.Background(), "read_file",
		args(t, map[string]string{"path": "hello.txt"}), asker(Decision{Allow: true}, new([]string)))
	if err != nil || isErr {
		t.Fatalf("read failed: %q %v", out, err)
	}
	for _, want := range []string{"line one", "line two"} {
		if !strings.Contains(out, want) {
			t.Fatalf("content missing %q: %q", want, out)
		}
	}
}
