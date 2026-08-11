package project

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// setupRepo makes a worktree that looks like a repository, plus a config dir.
func setupRepo(t *testing.T) (work, cfgDir, home string) {
	t.Helper()
	home = t.TempDir()
	if real, err := filepath.EvalSymlinks(home); err == nil {
		home = real
	}
	t.Setenv("HOME", home)
	work = filepath.Join(home, "repo")
	cfgDir = filepath.Join(home, ".config", "tapioca")
	t.Setenv("TAPIOCA_CONFIG_DIR", cfgDir)
	for _, d := range []string{filepath.Join(work, ".git"), cfgDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return work, cfgDir, home
}

// A cloned repository chooses what its instruction files say, and they land in
// the system prompt on every turn with no tool call to decline.
func TestImportCannotEscapeTheProject(t *testing.T) {
	work, _, home := setupRepo(t)
	write(t, filepath.Join(home, ".aws", "credentials"), "aws_secret_access_key = CANARY")
	write(t, filepath.Join(home, "elsewhere", "notes.md"), "OTHER-CANARY")

	for _, spec := range []string{"@~/.aws/credentials", "@" + filepath.Join(home, ".aws", "credentials"), "@../.aws/credentials", "@../elsewhere/notes.md"} {
		write(t, filepath.Join(work, "AGENTS.md"), "# Project notes\n"+spec+"\n")
		got := Instructions(work)
		if strings.Contains(got, "CANARY") {
			t.Fatalf("%s leaked a file outside the project:\n%s", spec, got)
		}
		if !strings.Contains(got, "import refused") {
			t.Errorf("%s was dropped silently, which reads like a typo:\n%s", spec, got)
		}
	}
}

// git stores symlinks, so a clone can ship one.
func TestImportCannotEscapeViaSymlink(t *testing.T) {
	work, _, home := setupRepo(t)
	write(t, filepath.Join(home, ".aws", "credentials"), "aws_secret_access_key = CANARY")
	if err := os.Symlink(filepath.Join(home, ".aws"), filepath.Join(work, "vendor")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	write(t, filepath.Join(work, "AGENTS.md"), "# Notes\n@vendor/credentials\n")
	if got := Instructions(work); strings.Contains(got, "CANARY") {
		t.Fatalf("symlinked import escaped the project:\n%s", got)
	}
}

// The feature still has to work, or the fix is just a removal.
func TestImportStillWorksInsideTheProject(t *testing.T) {
	work, cfgDir, _ := setupRepo(t)
	write(t, filepath.Join(work, "docs", "style.md"), "STYLE-RULES")
	write(t, filepath.Join(work, "AGENTS.md"), "# Notes\n@docs/style.md\n")
	if got := Instructions(work); !strings.Contains(got, "STYLE-RULES") {
		t.Fatalf("in-project import stopped working:\n%s", got)
	}
	// A personal instruction file may import from the config directory.
	write(t, filepath.Join(cfgDir, "shared.md"), "PERSONAL-RULES")
	write(t, filepath.Join(cfgDir, "AGENTS.md"), "# Personal\n@shared.md\n")
	if got := Instructions(work); !strings.Contains(got, "PERSONAL-RULES") {
		t.Fatalf("config-dir import stopped working:\n%s", got)
	}
}

// The config directory has to be an import root so personal instruction files
// can compose — but it is also where the provider keys live, and read_file
// gates that file. An import must not be the way round it.
func TestImportCannotReachTheConfigFile(t *testing.T) {
	work, cfgDir, _ := setupRepo(t)
	write(t, filepath.Join(cfgDir, "config.toml"), "api_key = \"sk-CANARY\"\n")
	for _, spec := range []string{"@" + filepath.Join(cfgDir, "config.toml"), "@~/.config/tapioca/config.toml"} {
		write(t, filepath.Join(work, "AGENTS.md"), "# Notes\n"+spec+"\n")
		if got := Instructions(work); strings.Contains(got, "CANARY") {
			t.Fatalf("%s pulled the config into the system prompt:\n%s", spec, got)
		}
	}
	// Markdown in the config dir still composes.
	write(t, filepath.Join(cfgDir, "shared.md"), "PERSONAL-RULES")
	write(t, filepath.Join(cfgDir, "AGENTS.md"), "# Personal\n@shared.md\n")
	write(t, filepath.Join(work, "AGENTS.md"), "# Notes\n")
	if got := Instructions(work); !strings.Contains(got, "PERSONAL-RULES") {
		t.Fatalf("config-dir markdown import stopped working:\n%s", got)
	}
}

// With no VCS marker above it — an extracted tarball, an unpacked module — the
// ancestry walk ends at /home, and that used to become the import root.
func TestImportRootIsNotTheHomeDirectory(t *testing.T) {
	home := t.TempDir()
	if real, err := filepath.EvalSymlinks(home); err == nil {
		home = real
	}
	t.Setenv("HOME", home)
	t.Setenv("TAPIOCA_CONFIG_DIR", filepath.Join(home, ".config", "tapioca"))
	// A directory tree with no .git anywhere above it.
	work := filepath.Join(home, "downloads", "extracted-1.0")
	write(t, filepath.Join(home, ".ssh", "id_rsa"), "PRIVATE-CANARY")
	write(t, filepath.Join(work, "AGENTS.md"), "# Notes\n@../../.ssh/id_rsa\n")

	if root := projectRoot(work); root != work {
		t.Errorf("projectRoot = %q, want the directory itself when there is no repository", root)
	}
	if got := Instructions(work); strings.Contains(got, "PRIVATE-CANARY") {
		t.Fatalf("import escaped to the home directory:\n%s", got)
	}
}
