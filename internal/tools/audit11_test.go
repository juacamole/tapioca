package tools

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// needRipgrep skips a test that has nothing to say without rg. CI has no
// ripgrep and this machine does, which is the whole reason the two backends
// drifted apart unnoticed — so the branch that is present has to be driven
// explicitly rather than through grep()'s "try rg, fall back" path.
func needRipgrep(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("rg"); err != nil {
		t.Skip("ripgrep is not installed here; the rg backend cannot be exercised")
	}
}

// grepTree lays out a small worktree and returns an executor rooted at it.
func grepTree(t *testing.T, files map[string]string) *Executor {
	t.Helper()
	dir := t.TempDir()
	if resolved, err := filepath.EvalSymlinks(dir); err == nil {
		dir = resolved
	}
	for rel, body := range files {
		p := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return NewExecutor(dir, ModeManual)
}

func joined(matches []string) string { return strings.Join(matches, "\n") }

// Hidden files are ordinary project files: .github/workflows, .golangci.yml,
// .tapioca/commands, .gitlab-ci.yml, .dockerignore. The Go walk searches them
// and ripgrep, left to itself, does not — rg skips dotfiles unless --hidden is
// given. grep therefore answered differently depending on whether rg happened
// to be installed, which is the same fault shape as the walk that could not
// descend a symlinked root: the machine with the richer tooling hides it, and
// here it is the machine that HAS ripgrep — nearly every developer — that gets
// the wrong answer.
func TestGrepFindsHiddenFilesWithEitherBackend(t *testing.T) {
	e := grepTree(t, map[string]string{
		"plain.txt":                "needle in plain sight",
		".github/workflows/ci.yml": "needle in the workflow",
		".golangci.yml":            "needle in the linter config",
	})
	ctx := context.Background()

	walk, _, err := e.grepWalk(ctx, "needle", e.Cwd(), "", false, 100)
	if err != nil {
		t.Fatal(err)
	}
	// The control: the fallback finds all three, so "rg found fewer" below is
	// about rg and not about the fixture.
	if len(walk) != 3 {
		t.Fatalf("the walk backend found %d matches, want 3: %s", len(walk), joined(walk))
	}

	needRipgrep(t)
	rg, _, err := e.grepRipgrep(ctx, "needle", e.Cwd(), "", false, 100)
	if err != nil {
		t.Fatalf("rg backend refused the search: %v", err)
	}
	for _, want := range []string{"ci.yml", ".golangci.yml"} {
		if !strings.Contains(joined(rg), want) {
			t.Errorf("the rg backend missed %s; the two backends disagree: %s", want, joined(rg))
		}
	}
}

// .git must stay out of both backends: it is in skipDirs, and --hidden would
// otherwise walk straight into it and return every blob rg can decode.
func TestGrepStillSkipsDotGitWithEitherBackend(t *testing.T) {
	e := grepTree(t, map[string]string{
		"plain.txt":       "needle in plain sight",
		".git/config":     "needle in the git config",
		".git/logs/HEAD":  "needle in the reflog",
		"sub/.git/config": "needle in a submodule git dir",
	})
	ctx := context.Background()
	for _, tc := range []struct {
		name string
		run  func() ([]string, bool, error)
	}{
		{"walk", func() ([]string, bool, error) { return e.grepWalk(ctx, "needle", e.Cwd(), "", false, 100) }},
		{"rg", func() ([]string, bool, error) { return e.grepRipgrep(ctx, "needle", e.Cwd(), "", false, 100) }},
	} {
		if tc.name == "rg" {
			needRipgrep(t)
		}
		got, _, err := tc.run()
		if err != nil {
			t.Fatalf("%s backend: %v", tc.name, err)
		}
		if !strings.Contains(joined(got), "plain.txt") {
			t.Fatalf("%s backend found nothing at all: %s", tc.name, joined(got))
		}
		if strings.Contains(joined(got), ".git") {
			t.Errorf("%s backend returned something from .git: %s", tc.name, joined(got))
		}
	}
}

// A long line is clipped, not dropped. rg's --max-columns replaces the whole
// line with "[Omitted long matching line]" unless --max-columns-preview is
// given, so a match in a minified bundle, a generated file or a one-line JSON
// fixture came back as a marker with no content — while the fallback returned
// the first maxLineWidth characters. Same query, two answers.
func TestGrepClipsLongLinesWithEitherBackend(t *testing.T) {
	long := "needle " + strings.Repeat("x", maxLineWidth*2)
	e := grepTree(t, map[string]string{"bundle.min.js": long + "\n"})
	ctx := context.Background()

	walk, _, err := e.grepWalk(ctx, "needle", e.Cwd(), "", false, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(walk) != 1 || !strings.Contains(walk[0], "needle xxxx") {
		t.Fatalf("the walk backend did not clip-and-return the long line: %q", joined(walk))
	}

	needRipgrep(t)
	rg, _, err := e.grepRipgrep(ctx, "needle", e.Cwd(), "", false, 100)
	if err != nil {
		t.Fatalf("rg backend refused the search: %v", err)
	}
	if len(rg) != 1 {
		t.Fatalf("the rg backend returned %d matches, want 1: %s", len(rg), joined(rg))
	}
	if !strings.Contains(rg[0], "needle xxxx") {
		t.Errorf("the rg backend dropped the content of the long line: %q", rg[0])
	}
}

// The secret filter must survive a colon in a path. rg prints file:line:text
// and the reader cut at the first colon, so a directory named with one — and
// directory names in an extracted tarball are the attacker's to choose — moved
// the cut left of the filename. sensitivePath then judged a harmless prefix
// ("<root>/a") instead of the real path ("<root>/a:b/id_rsa"), and the filter
// that exists to keep keys out of search output stopped firing. The walk
// backend, which sees the whole path, blocks the same file.
func TestGrepSecretFilterSurvivesAColonInThePath(t *testing.T) {
	e := grepTree(t, map[string]string{
		"a:b/id_rsa":   "needle -----BEGIN OPENSSH PRIVATE KEY-----",
		"a:b/notes.md": "needle in an ordinary file",
	})
	ctx := context.Background()

	walk, _, err := e.grepWalk(ctx, "needle", e.Cwd(), "", false, 100)
	if err != nil {
		t.Fatal(err)
	}
	// Two controls: the filter can fire here at all, and the colon directory is
	// otherwise searchable — so "id_rsa is missing" is the filter working and
	// not the whole subtree being skipped.
	if strings.Contains(joined(walk), "id_rsa") {
		t.Fatal("the walk backend returned id_rsa; the secret filter does not fire here and the test would be vacuous")
	}
	if !strings.Contains(joined(walk), "notes.md") {
		t.Fatalf("the walk backend could not search the colon directory at all: %s", joined(walk))
	}

	needRipgrep(t)
	rg, _, err := e.grepRipgrep(ctx, "needle", e.Cwd(), "", false, 100)
	if err != nil {
		t.Fatalf("rg backend refused the search: %v", err)
	}
	if !strings.Contains(joined(rg), "notes.md") {
		t.Fatalf("the rg backend could not search the colon directory at all: %s", joined(rg))
	}
	if strings.Contains(joined(rg), "BEGIN OPENSSH PRIVATE KEY") {
		t.Errorf("the rg backend returned the contents of id_rsa past the secret filter: %s", joined(rg))
	}
}

// Ordinary use: a path with a colon in it still comes back spelled correctly,
// and a glob still matches it. Both backends are asked the same question.
func TestGrepReportsColonPathsCorrectly(t *testing.T) {
	e := grepTree(t, map[string]string{"docs/rfc:7231.md": "needle in a colon file"})
	ctx := context.Background()
	want := filepath.Join("docs", "rfc:7231.md") + ":1:"

	walk, _, err := e.grepWalk(ctx, "needle", e.Cwd(), "", false, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(walk) != 1 || !strings.HasPrefix(walk[0], want) {
		t.Fatalf("the walk backend spelled the path wrong: %q, want prefix %q", joined(walk), want)
	}

	needRipgrep(t)
	rg, _, err := e.grepRipgrep(ctx, "needle", e.Cwd(), "", false, 100)
	if err != nil {
		t.Fatalf("rg backend refused the search: %v", err)
	}
	if len(rg) != 1 || !strings.HasPrefix(rg[0], want) {
		t.Errorf("the rg backend spelled the path wrong: %q, want prefix %q", joined(rg), want)
	}
}
