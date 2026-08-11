package gitcmd

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// A repository's .git/config names programs git will run. core.fsmonitor is
// the sharpest: `git status` executes it, and the git panel polls status every
// few seconds, so opening a directory — in any mode, including plan, before
// the user has typed anything — ran whatever that directory chose.
func TestRepoConfigCannotExecuteAProgram(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	dir := t.TempDir()
	marker := filepath.Join(dir, "EXECUTED")
	hook := filepath.Join(dir, "hook.sh")
	if err := os.WriteFile(hook, []byte("#!/bin/sh\ntouch "+marker+"\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"init", "-q", "."},
		{"config", "core.fsmonitor", hook},
	} {
		if out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("setup %v: %v %s", args, err, out)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	// The unhardened form is what used to run, and it is the control: if this
	// stops executing the hook, the test is no longer proving anything.
	_ = exec.Command("git", "-C", dir, "status", "--porcelain", "-b").Run()
	if _, err := os.Stat(marker); err != nil {
		t.Skip("this git build does not run core.fsmonitor; the test cannot prove anything")
	}
	if err := os.Remove(marker); err != nil {
		t.Fatal(err)
	}

	_ = In(dir, "status", "--porcelain", "-b").Run()
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("the hardened command still executed the repository's fsmonitor program")
	}
}

// The hardening must not stop git from answering.
func TestHardenedGitStillWorks(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	dir := t.TempDir()
	for _, args := range [][]string{
		{"init", "-q", "."},
		{"config", "user.email", "t@example.com"},
		{"config", "user.name", "t"},
	} {
		if out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("setup: %v %s", err, out)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := In(dir, "status", "--porcelain", "-b").Output()
	if err != nil {
		t.Fatalf("hardened status failed: %v", err)
	}
	if len(out) == 0 {
		t.Fatal("hardened status returned nothing")
	}
}
