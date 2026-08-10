package project

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// repo builds a worktree with a .git marker so the ancestry walk has a root.
func repo(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	// Keep the global file out of the way unless a test asks for it.
	t.Setenv("TAPIOCA_CONFIG_DIR", t.TempDir())
	return root
}

func write(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// Working in a subdirectory has to pick up the repository's instructions;
// before this, a subdirectory got nothing at all.
func TestInstructionsWalkUpToTheRepoRoot(t *testing.T) {
	root := repo(t)
	write(t, filepath.Join(root, "AGENTS.md"), "root rules")
	sub := filepath.Join(root, "internal", "deep")
	write(t, filepath.Join(sub, "AGENTS.md"), "deep rules")

	got := Instructions(sub)
	if !strings.Contains(got, "root rules") || !strings.Contains(got, "deep rules") {
		t.Fatalf("missing a level:\n%s", got)
	}
	// The nearest file speaks last, so it wins where they disagree.
	if strings.Index(got, "root rules") > strings.Index(got, "deep rules") {
		t.Errorf("the nearer file should come last:\n%s", got)
	}
}

// A file above the repository root is somebody else's business.
func TestWalkStopsAtTheRepoRoot(t *testing.T) {
	outer := t.TempDir()
	t.Setenv("TAPIOCA_CONFIG_DIR", t.TempDir())
	write(t, filepath.Join(outer, "AGENTS.md"), "unrelated outer file")
	root := filepath.Join(outer, "project")
	write(t, filepath.Join(root, ".git", "HEAD"), "ref: refs/heads/main\n")
	write(t, filepath.Join(root, "AGENTS.md"), "project rules")

	got := Instructions(root)
	if !strings.Contains(got, "project rules") {
		t.Fatalf("project file missing:\n%s", got)
	}
	if strings.Contains(got, "unrelated outer file") {
		t.Errorf("walked past the repo root:\n%s", got)
	}
}

// Someone else's repo documents itself for another agent; read that rather
// than starting with nothing.
func TestOtherToolsInstructionFilesAreRead(t *testing.T) {
	root := repo(t)
	write(t, filepath.Join(root, "CLAUDE.md"), "claude conventions")
	write(t, filepath.Join(root, "GEMINI.md"), "gemini conventions")

	got := Instructions(root)
	for _, want := range []string{"claude conventions", "gemini conventions"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q:\n%s", want, got)
		}
	}
}

func TestGlobalInstructionsApplyEverywhere(t *testing.T) {
	root := repo(t)
	write(t, filepath.Join(GlobalDir(), "AGENTS.md"), "always be terse")
	write(t, filepath.Join(root, "AGENTS.md"), "project rules")

	got := Instructions(root)
	if !strings.Contains(got, "always be terse") {
		t.Fatalf("global file not read:\n%s", got)
	}
	// Global first, so the project can override it.
	if strings.Index(got, "always be terse") > strings.Index(got, "project rules") {
		t.Errorf("global should come before the project:\n%s", got)
	}
}

func TestImportsAreInlined(t *testing.T) {
	root := repo(t)
	write(t, filepath.Join(root, "docs", "style.md"), "two spaces, never tabs")
	write(t, filepath.Join(root, "AGENTS.md"), "see below\n@docs/style.md\ntrailing line")

	got := Instructions(root)
	if !strings.Contains(got, "two spaces, never tabs") {
		t.Fatalf("import not inlined:\n%s", got)
	}
	if strings.Contains(got, "@docs/style.md") {
		t.Errorf("the import line survived:\n%s", got)
	}
	if !strings.Contains(got, "trailing line") {
		t.Errorf("content after the import was lost:\n%s", got)
	}
}

// An email address or a mention is not an import.
func TestOnlyBareImportLinesCount(t *testing.T) {
	root := repo(t)
	write(t, filepath.Join(root, "AGENTS.md"), "ask @someone before merging\ncontact a@b.com")
	got := Instructions(root)
	if !strings.Contains(got, "ask @someone before merging") || !strings.Contains(got, "a@b.com") {
		t.Errorf("prose containing @ was mangled:\n%s", got)
	}
}

func TestImportCyclesTerminate(t *testing.T) {
	root := repo(t)
	write(t, filepath.Join(root, "AGENTS.md"), "start\n@loop.md")
	write(t, filepath.Join(root, "loop.md"), "looped\n@AGENTS.md")

	done := make(chan string, 1)
	go func() { done <- Instructions(root) }()
	select {
	case got := <-done:
		if !strings.Contains(got, "looped") {
			t.Errorf("import not resolved:\n%s", got)
		}
	case <-timeoutAfter():
		t.Fatal("a cyclic import did not terminate")
	}
}

func TestEachFileIsReadOnce(t *testing.T) {
	root := repo(t)
	write(t, filepath.Join(root, "AGENTS.md"), "unique-marker")
	got := Instructions(root)
	if n := strings.Count(got, "unique-marker"); n != 1 {
		t.Errorf("file included %d times:\n%s", n, got)
	}
}

func TestNoInstructionsIsEmpty(t *testing.T) {
	if got := Instructions(repo(t)); got != "" {
		t.Errorf("expected nothing, got:\n%s", got)
	}
}

func timeoutAfter() <-chan time.Time { return time.After(5 * time.Second) }
