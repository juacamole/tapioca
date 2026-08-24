package gitcmd

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// git has two environment channels for what it calls command-line config, not
// one. This package delivers every pin through the numbered one —
// GIT_CONFIG_COUNT with GIT_CONFIG_KEY_<n>/GIT_CONFIG_VALUE_<n> — and
// configEnv takes care to number the pins after anything already there so they
// win. The other channel, GIT_CONFIG_PARAMETERS, is read after the numbered
// pairs and therefore outranks all of them, and nothing here touched it.
//
// So the pin and the value it exists to overrule were never in the same
// precedence form, and the pin could not win however it was numbered: one
// variable in the environment voids the whole file. `git status --porcelain`
// is what the git panel polls every five seconds, in every permission mode,
// before the user has typed anything.
//
// The variable is in the threat model the same way XDG_CONFIG_HOME is (see
// config/hooks.go): an .envrc in an extracted tree exports it, direnv puts it
// in the shell, and Tapioca inherits it — secretenv.Scrubbed removes provider
// keys and passes every GIT_* through.
func TestGitConfigParametersCannotOutrankThePins(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	dir := t.TempDir()
	marker := filepath.Join(dir, "RAN")
	for _, args := range [][]string{
		{"init", "-q", "."}, {"config", "user.email", "t@e.com"}, {"config", "user.name", "t"},
	} {
		if out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("setup %v: %v %s", args, err, out)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "f.txt"), []byte("hi\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// In the process environment, which is where an .envrc puts it and what
	// secretenv.Scrubbed hands to every git child. git runs core.fsmonitor
	// through a shell, so this is one command line — no $'…' and no bashisms,
	// since /bin/sh is dash on CI.
	t.Setenv("GIT_CONFIG_PARAMETERS", "'core.fsmonitor=touch "+marker+"; false'")

	// The control: with the pins out of the way the channel really does execute
	// here. Without this, "the marker is absent" below could mean this git
	// ignores core.fsmonitor, or runs it some other way, and the test would
	// assert nothing.
	if !runs(t, dir, marker, func() {
		cmd := exec.Command("git", "-C", dir, "status", "--porcelain")
		cmd.Env = os.Environ()
		_ = cmd.Run()
	}) {
		t.Skip("this git does not execute core.fsmonitor from GIT_CONFIG_PARAMETERS here")
	}

	if runs(t, dir, marker, func() { _ = In(dir, "status", "--porcelain").Run() }) {
		t.Error("a program named through GIT_CONFIG_PARAMETERS ran despite the pin for the same key")
	}
}

// The pins are delivered by configEnv, so a caller that builds its own git
// invocation through WithPins — the checkpoint repository does — answers to the
// same thing.
func TestWithPinsAlsoDropsGitConfigParameters(t *testing.T) {
	env := WithPins([]string{
		"PATH=/usr/bin",
		"GIT_CONFIG_PARAMETERS='core.pager=ATTACKER'",
	}, Pin{Key: "core.pager", Value: "cat"})
	for _, kv := range env {
		if strings.HasPrefix(kv, "GIT_CONFIG_PARAMETERS=") {
			t.Errorf("GIT_CONFIG_PARAMETERS survived into a pinned environment: %q", kv)
		}
	}
	// Ordinary use: everything else is left alone, and the pin is still there.
	if !contains(env, "PATH=/usr/bin") {
		t.Error("configEnv dropped an unrelated variable")
	}
	if !contains(env, "GIT_CONFIG_KEY_0=core.pager") || !contains(env, "GIT_CONFIG_VALUE_0=cat") {
		t.Errorf("the pin is missing from %v", env)
	}
}

// Ordinary use, and the behaviour configEnv was written for: numbered pairs the
// user exported are kept, and the pins are numbered after them so they still
// win. Only the channel that cannot be outranked is dropped.
func TestNumberedConfigPairsAreStillHonoured(t *testing.T) {
	env := WithPins([]string{
		"GIT_CONFIG_COUNT=1",
		"GIT_CONFIG_KEY_0=user.name",
		"GIT_CONFIG_VALUE_0=someone",
	}, Pin{Key: "core.pager", Value: "cat"})
	for _, want := range []string{
		"GIT_CONFIG_KEY_0=user.name", "GIT_CONFIG_VALUE_0=someone",
		"GIT_CONFIG_KEY_1=core.pager", "GIT_CONFIG_VALUE_1=cat",
		"GIT_CONFIG_COUNT=2",
	} {
		if !contains(env, want) {
			t.Errorf("missing %q from %v", want, env)
		}
	}
}

func contains(env []string, want string) bool {
	for _, kv := range env {
		if kv == want {
			return true
		}
	}
	return false
}

// A repository can also reach git through the environment rather than through
// its own config, and the checkpoint snapshot is the git run that touches the
// worktree hardest: `git add -A` is exactly what a clean filter fires on.
func TestCheckpointStyleRunAlsoResistsGitConfigParameters(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	dir := t.TempDir()
	marker := filepath.Join(dir, "RAN")
	if out, err := exec.Command("git", "-C", dir, "init", "-q", ".").CombinedOutput(); err != nil {
		t.Fatalf("setup: %v %s", err, out)
	}
	if err := os.WriteFile(filepath.Join(dir, "f.txt"), []byte("hi\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(filepath.Join(dir, "f.txt"), time.Unix(1, 0), time.Unix(1, 0)); err != nil {
		t.Fatal(err)
	}
	injected := "GIT_CONFIG_PARAMETERS='core.fsmonitor=touch " + marker + "; false'"

	if !runs(t, dir, marker, func() {
		cmd := exec.Command("git", "-C", dir, "status", "--porcelain")
		cmd.Env = append(os.Environ(), injected)
		_ = cmd.Run()
	}) {
		t.Skip("this git does not execute core.fsmonitor from GIT_CONFIG_PARAMETERS here")
	}
	if runs(t, dir, marker, func() {
		cmd := exec.Command("git", "-C", dir, "status", "--porcelain")
		cmd.Env = WithPins(append(os.Environ(), injected), StaticPins()...)
		_ = cmd.Run()
	}) {
		t.Error("WithPins did not stop a program named through GIT_CONFIG_PARAMETERS")
	}
}
