package ui

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"tapioca/internal/config"
)

// End to end for the editor key: a committed config names the program, and
// opening the external editor is what runs it. resolveEditor is the whole
// distance between the config value and an argv, so running that argv here is
// what openEditorCmd does one line later.
func TestCommittedConfigCannotRunItsOwnEditor(t *testing.T) {
	t.Setenv("HOME", t.TempDir()) // the user's own config is elsewhere
	tree := t.TempDir()
	if err := os.MkdirAll(filepath.Join(tree, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(tree, "pwned")
	script := filepath.Join(tree, "pwn.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\ntouch "+marker+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(tree, "config.toml")
	if err := os.WriteFile(cfgPath, []byte("editor = \"sh "+script+"\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}

	// Control: the value really does become the program that runs, so a test
	// that finds no marker below found it because the key was dropped.
	argv, err := resolveEditor(cfg.Editor)
	if err != nil {
		t.Fatal(err)
	}
	if err := exec.Command(argv[0], append(argv[1:], "/dev/null")...).Run(); err != nil {
		t.Fatalf("control failed: the editor argv did not run: %v", err)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("control failed: the editor key does not decide what runs: %v", err)
	}
	os.Remove(marker)

	// Nothing is run a second time on purpose: with the key dropped,
	// resolveEditor falls through to $EDITOR and then to vi, and this test is
	// not entitled to open the real one. What it asserts is that the committed
	// value is no longer what would be run.
	cfg.RestrictIfInsideTree(tree)
	if cfg.Editor != "" {
		t.Errorf("a committed config still names the program the editor opens: %q", cfg.Editor)
	}
}
