package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

func searchTree(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	files := map[string]string{
		"main.go":               "package main\n\nfunc needle() {}\n",
		"internal/app/app.go":   "package app\n\n// needle lives here too\n",
		"internal/app/app.txt":  "no match here\n",
		"docs/guide.md":         "the needle in prose\n",
		"node_modules/dep.go":   "func needle() {} // must be skipped\n",
		".env":                  "API_TOKEN=needle-secret\n",
		"internal/app/deep.log": "needle\n",
	}
	for rel, body := range files {
		path := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func callJSON(t *testing.T, e *Executor, name string, args any) string {
	t.Helper()
	raw, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	out, isErr, err := e.Call(context.Background(), name, raw, func(string, string) Decision {
		t.Fatalf("%s prompted unexpectedly", name)
		return Decision{}
	})
	if err != nil {
		t.Fatal(err)
	}
	if isErr {
		t.Fatalf("%s failed: %s", name, out)
	}
	return out
}

func TestGrepFindsMatchesAndSkipsNoise(t *testing.T) {
	root := searchTree(t)
	e := NewExecutor(root, ModeManual)

	out := callJSON(t, e, "grep", map[string]any{"pattern": "needle"})
	for _, want := range []string{"main.go", "app.go", "guide.md"} {
		if !strings.Contains(out, want) {
			t.Errorf("grep missed %s:\n%s", want, out)
		}
	}
	if strings.Contains(out, "node_modules") {
		t.Errorf("grep searched node_modules:\n%s", out)
	}
	if strings.Contains(out, "needle-secret") {
		t.Errorf("grep leaked .env contents:\n%s", out)
	}
	if !strings.Contains(out, ":3:") && !strings.Contains(out, ":1:") {
		t.Errorf("grep output has no line numbers:\n%s", out)
	}

	out = callJSON(t, e, "grep", map[string]any{"pattern": "needle", "glob": "*.md"})
	if !strings.Contains(out, "guide.md") || strings.Contains(out, "main.go") {
		t.Errorf("glob filter ignored:\n%s", out)
	}

	if out := callJSON(t, e, "grep", map[string]any{"pattern": "zzz-nothing-zzz"}); out != "no matches" {
		t.Errorf("expected no matches, got %q", out)
	}
}

// CI may not have ripgrep, so the two backends are exercised separately and
// asserted to agree; a silent divergence would make results depend on the host.
func TestGrepBackendsAgree(t *testing.T) {
	root := searchTree(t)
	e := NewExecutor(root, ModeManual)
	ctx := context.Background()

	rgOut, _, err := e.grepRipgrep(ctx, "needle", root, "", false, defaultMatches)
	if err != nil {
		t.Skip("ripgrep not installed")
	}
	goOut, _, err := e.grepWalk(ctx, "needle", root, "", false, defaultMatches)
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(rgOut)
	sort.Strings(goOut)
	if strings.Join(rgOut, "\n") != strings.Join(goOut, "\n") {
		t.Errorf("backends disagree:\nripgrep:\n%s\n\nwalk:\n%s",
			strings.Join(rgOut, "\n"), strings.Join(goOut, "\n"))
	}
}

func TestGrepWalkFallbackSkipsNoise(t *testing.T) {
	root := searchTree(t)
	e := NewExecutor(root, ModeManual)
	matches, _, err := e.grepWalk(context.Background(), "needle", root, "", false, defaultMatches)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(matches, "\n")
	if strings.Contains(joined, "node_modules") || strings.Contains(joined, "needle-secret") {
		t.Errorf("fallback searched excluded files:\n%s", joined)
	}
	if !strings.Contains(joined, "main.go") {
		t.Errorf("fallback missed main.go:\n%s", joined)
	}
}

func TestGlobMatchesAcrossDirectories(t *testing.T) {
	root := searchTree(t)
	e := NewExecutor(root, ModeManual)

	out := callJSON(t, e, "glob", map[string]any{"pattern": "**/*.go"})
	if !strings.Contains(out, "main.go") || !strings.Contains(out, filepath.Join("internal", "app", "app.go")) {
		t.Errorf("glob missed go files:\n%s", out)
	}
	if strings.Contains(out, "node_modules") {
		t.Errorf("glob returned node_modules:\n%s", out)
	}

	// A bare pattern searches everywhere, not just the root.
	if out := callJSON(t, e, "glob", map[string]any{"pattern": "*.md"}); !strings.Contains(out, "guide.md") {
		t.Errorf("bare pattern did not recurse:\n%s", out)
	}
	if out := callJSON(t, e, "glob", map[string]any{"pattern": "internal/**/*.txt"}); !strings.Contains(out, "app.txt") {
		t.Errorf("prefixed ** pattern failed:\n%s", out)
	}
}

func TestSearchOutsideWorktreePrompts(t *testing.T) {
	root := searchTree(t)
	outside := t.TempDir()
	e := NewExecutor(root, ModeManual)

	for _, name := range []string{"grep", "glob"} {
		var log []string
		raw, _ := json.Marshal(map[string]any{"pattern": "needle", "path": outside})
		out, isErr, err := e.Call(context.Background(), name, raw, asker(Decision{}, &log))
		if err != nil {
			t.Fatal(err)
		}
		if !isErr || len(log) != 1 {
			t.Errorf("%s searched outside the worktree without a prompt: out=%q asked=%v", name, out, log)
		}
	}
}

func TestGlobMatchSegments(t *testing.T) {
	cases := []struct {
		pattern, path string
		want          bool
	}{
		{"**/*.go", "main.go", true},
		{"**/*.go", "a/b/main.go", true},
		{"*.go", "a/main.go", false},
		{"internal/**/*_test.go", "internal/tools/x_test.go", true},
		{"internal/**/*_test.go", "cmd/x_test.go", false},
		{"**", "any/thing.txt", true},
		{"a/*/c", "a/b/c", true},
		{"a/*/c", "a/b/d/c", false},
	}
	for _, c := range cases {
		if got := globMatch(c.pattern, c.path); got != c.want {
			t.Errorf("globMatch(%q, %q) = %v, want %v", c.pattern, c.path, got, c.want)
		}
	}
}
