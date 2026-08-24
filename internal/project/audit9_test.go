package project

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// "Inside the project" is a statement about the path, and the read is about
// the inode. A symlink makes the two disagree and resolveReal settles it; a
// hard link makes them disagree with nothing to resolve. `ln
// ~/.config/tapioca/config.toml keys.md` gives the provider keys a name that
// is genuinely inside the project and genuinely ends in ".md", so both halves
// of the round-8 test pass and the read still gets config.toml — sent to the
// provider on every turn, in every mode, with no tool call to decline.
func TestAHardLinkCannotCarryTheConfigIntoTheProject(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("hard links are not the same thing on windows")
	}
	base := t.TempDir()
	cfgDir := filepath.Join(base, "cfg")
	proj := filepath.Join(base, "proj")
	for _, d := range []string{cfgDir, proj} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("TAPIOCA_CONFIG_DIR", cfgDir)
	secret := filepath.Join(cfgDir, "config.toml")
	if err := os.WriteFile(secret, []byte("api_key = \"SECRET-KEY\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(proj, ".git"), []byte("gitdir: x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(proj, "keys.md")
	if err := os.Link(secret, alias); err != nil {
		t.Skipf("cannot hard link here: %v", err)
	}
	if err := os.WriteFile(filepath.Join(proj, "AGENTS.md"), []byte("hello\n@keys.md\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// The control: both halves of the round-8 test say yes to this file, so
	// what stops it has to be something else.
	roots := importRoots{proj}
	if !roots.allows(alias) {
		t.Fatal("the root test already refused the alias; the test proves nothing")
	}
	if !importable(resolveReal(alias)) {
		t.Fatal("the extension test already refused the alias; the test proves nothing")
	}

	if out := Instructions(proj); strings.Contains(out, "SECRET-KEY") {
		t.Errorf("the config reached the system prompt through a hard link:\n%s", out)
	}

	// And an ordinary import beside it still works, so the refusal is about
	// the alias and not about imports.
	if err := os.WriteFile(filepath.Join(proj, "more.md"), []byte("extra guidance\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(proj, "AGENTS.md"), []byte("hello\n@more.md\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if out := Instructions(proj); !strings.Contains(out, "extra guidance") {
		t.Errorf("an ordinary import stopped working:\n%s", out)
	}
}

// The link count is the gate on that walk, and a project file can have more
// than one name for reasons that have nothing to do with the config directory
// — an rsync --link-dest backup, a `cp -al` snapshot. Those must still be
// read, or the fix is a silent refusal of somebody's instructions.
func TestAProjectFileWithASecondNameOfItsOwnIsStillRead(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("hard links are not the same thing on windows")
	}
	base := t.TempDir()
	cfgDir := filepath.Join(base, "cfg")
	proj := filepath.Join(base, "proj")
	snapshot := filepath.Join(base, "snapshot")
	for _, d := range []string{cfgDir, proj, snapshot} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("TAPIOCA_CONFIG_DIR", cfgDir)
	if err := os.WriteFile(filepath.Join(cfgDir, "config.toml"), []byte("api_key = \"K\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(proj, ".git"), []byte("gitdir: x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	agents := filepath.Join(proj, "AGENTS.md")
	if err := os.WriteFile(agents, []byte("project guidance\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(agents, filepath.Join(snapshot, "AGENTS.md")); err != nil {
		t.Skipf("cannot hard link here: %v", err)
	}
	if out := Instructions(proj); !strings.Contains(out, "project guidance") {
		t.Errorf("a backed-up project file was refused:\n%s", out)
	}
}
