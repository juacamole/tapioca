package ui

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeCmd(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestLoadUserCommandsFromBothPlaces(t *testing.T) {
	cfgDir := t.TempDir()
	t.Setenv("TAPIOCA_CONFIG_DIR", cfgDir)
	work := t.TempDir()

	writeCmd(t, filepath.Join(cfgDir, "commands"), "review.md", "# Review the diff\nLook at the staged changes.")
	writeCmd(t, filepath.Join(work, ".tapioca", "commands"), "ship.md", "# Ship it\nRun the tests, then commit.")

	cmds := loadUserCmds(work)
	if len(cmds) != 2 {
		t.Fatalf("loaded %d commands: %+v", len(cmds), cmds)
	}
	byName := map[string]userCmd{}
	for _, c := range cmds {
		byName[c.name] = c
	}
	if byName["review"].desc != "Review the diff" {
		t.Errorf("heading not used as the description: %+v", byName["review"])
	}
	if byName["review"].body != "Look at the staged changes." {
		t.Errorf("heading not stripped from the body: %q", byName["review"].body)
	}
	if !byName["ship"].project {
		t.Error("the worktree command should be marked as a project one")
	}
}

// A project command with the same name is the one that should run.
func TestProjectCommandsOverridePersonalOnes(t *testing.T) {
	cfgDir := t.TempDir()
	t.Setenv("TAPIOCA_CONFIG_DIR", cfgDir)
	work := t.TempDir()
	writeCmd(t, filepath.Join(cfgDir, "commands"), "test.md", "personal version")
	writeCmd(t, filepath.Join(work, ".tapioca", "commands"), "test.md", "project version")

	cmds := loadUserCmds(work)
	if len(cmds) != 1 {
		t.Fatalf("expected one command, got %+v", cmds)
	}
	if !strings.Contains(cmds[0].body, "project version") {
		t.Errorf("personal version won: %+v", cmds[0])
	}
}

func TestRenderSubstitutesArguments(t *testing.T) {
	c := userCmd{body: "Explain " + argsToken + " in one paragraph."}
	if got := c.render("the retry layer"); got != "Explain the retry layer in one paragraph." {
		t.Errorf("got %q", got)
	}
	// Missing arguments leave the sentence intact rather than the token.
	if got := c.render(""); strings.Contains(got, argsToken) {
		t.Errorf("token survived with no arguments: %q", got)
	}
}

// A template written without the token should still receive what was typed,
// rather than silently discarding it.
func TestRenderAppendsArgumentsWithoutAToken(t *testing.T) {
	c := userCmd{body: "Review the diff."}
	got := c.render("focus on error handling")
	if !strings.Contains(got, "Review the diff.") || !strings.Contains(got, "focus on error handling") {
		t.Errorf("arguments dropped: %q", got)
	}
	if got := c.render(""); got != "Review the diff." {
		t.Errorf("empty arguments changed the body: %q", got)
	}
}

func TestIgnoresNonCommandFiles(t *testing.T) {
	cfgDir := t.TempDir()
	t.Setenv("TAPIOCA_CONFIG_DIR", cfgDir)
	dir := filepath.Join(cfgDir, "commands")
	writeCmd(t, dir, "notes.txt", "not markdown")
	writeCmd(t, dir, "empty.md", "   ")
	writeCmd(t, dir, "good.md", "fine")
	if err := os.MkdirAll(filepath.Join(dir, "subdir.md"), 0o755); err != nil {
		t.Fatal(err)
	}

	cmds := loadUserCmds(t.TempDir())
	if len(cmds) != 1 || cmds[0].name != "good" {
		t.Fatalf("expected only the good command, got %+v", cmds)
	}
}

func TestNoCommandDirectoryIsFine(t *testing.T) {
	t.Setenv("TAPIOCA_CONFIG_DIR", t.TempDir())
	if cmds := loadUserCmds(t.TempDir()); len(cmds) != 0 {
		t.Errorf("expected none, got %+v", cmds)
	}
}
