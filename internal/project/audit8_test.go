package project

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The config directory has to be an import root, so a personal instruction
// file can compose out of several markdown files. It is also where the
// provider keys and the MCP bearer tokens live, and importable() is the only
// thing keeping config.toml out of the system prompt.
//
// importable() is asked about the path as written, while the read follows it.
// git stores symlinks, so a clone ships one named with a .md extension and
// pointing into the config directory: the extension check sees markdown, the
// root check sees a file in a root it allows, and the open sees config.toml.
// The whole file then goes to the provider on every turn, in every mode
// including plan, with no tool call for anyone to decline.
func TestImportedSymlinkCannotReachTheConfigFile(t *testing.T) {
	work, cfgDir, _ := setupRepo(t)
	write(t, filepath.Join(cfgDir, "config.toml"), "api_key = \"sk-CANARY\"\n")
	if err := os.Symlink(filepath.Join(cfgDir, "config.toml"), filepath.Join(work, "keys.md")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	write(t, filepath.Join(work, "AGENTS.md"), "# Notes\n@keys.md\n")
	if got := Instructions(work); strings.Contains(got, "CANARY") {
		t.Fatalf("a .md symlink pointing into the config directory pulled the config into the system prompt:\n%s", got)
	}
}

// The same door with the import step left out: the instruction file itself is
// only checked against the roots, and the config directory is one of them.
func TestInstructionFileCannotBeASymlinkToTheConfigFile(t *testing.T) {
	work, cfgDir, _ := setupRepo(t)
	write(t, filepath.Join(cfgDir, "config.toml"), "api_key = \"sk-CANARY\"\n")
	if err := os.Symlink(filepath.Join(cfgDir, "config.toml"), filepath.Join(work, "AGENTS.md")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if got := Instructions(work); strings.Contains(got, "CANARY") {
		t.Fatalf("a symlinked AGENTS.md read the config file:\n%s", got)
	}
}

// The data directory holds every conversation ever had, and read_file gates it
// for the same reason. It is not an import root, so this is a check that the
// roots really are the only way in.
func TestImportCannotReachTheDataDirectory(t *testing.T) {
	work, _, home := setupRepo(t)
	data := filepath.Join(home, ".local", "share", "tapioca")
	t.Setenv("TAPIOCA_DATA_DIR", data)
	write(t, filepath.Join(data, "sessions", "one.md"), "SESSION-CANARY")
	if err := os.Symlink(filepath.Join(data, "sessions", "one.md"), filepath.Join(work, "hist.md")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	write(t, filepath.Join(work, "AGENTS.md"), "# Notes\n@hist.md\n")
	if got := Instructions(work); strings.Contains(got, "SESSION-CANARY") {
		t.Fatalf("an import read the data directory:\n%s", got)
	}
}

// Composing personal instruction files is the reason the config directory is a
// root, and it has to keep working — including through a symlink the user made
// themselves inside their own config directory.
func TestConfigDirectoryMarkdownStillComposes(t *testing.T) {
	work, cfgDir, _ := setupRepo(t)
	write(t, filepath.Join(cfgDir, "shared.md"), "PERSONAL-RULES")
	if err := os.Symlink(filepath.Join(cfgDir, "shared.md"), filepath.Join(cfgDir, "linked.md")); err == nil {
		write(t, filepath.Join(cfgDir, "AGENTS.md"), "# Personal\n@shared.md\n@linked.md\n")
	} else {
		write(t, filepath.Join(cfgDir, "AGENTS.md"), "# Personal\n@shared.md\n")
	}
	write(t, filepath.Join(work, "AGENTS.md"), "# Notes\n")
	if got := Instructions(work); !strings.Contains(got, "PERSONAL-RULES") {
		t.Fatalf("config-dir markdown import stopped working:\n%s", got)
	}
}
