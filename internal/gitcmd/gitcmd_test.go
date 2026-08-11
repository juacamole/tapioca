package gitcmd

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
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

// runs reports whether the given git invocation executed the repo's program.
func runs(t *testing.T, dir, marker string, run func()) bool {
	t.Helper()
	os.Remove(marker)
	run()
	_, err := os.Stat(marker)
	return err == nil
}

// A fixed -c list cannot pre-empt filter.<name>.clean, because the repository
// invents the name and selects it with its own .gitattributes. git runs the
// clean filter during `status` whenever stat data does not match the index,
// which is the normal state after extracting a tarball or copying a tree.
func TestCleanFilterCannotExecute(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	dir := t.TempDir()
	marker := filepath.Join(dir, "FILTER_RAN")
	hook := filepath.Join(dir, "pwn.sh")
	if err := os.WriteFile(hook, []byte("#!/bin/sh\ntouch "+marker+"\ncat\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(".gitattributes", "tracked.txt filter=evil\n")
	write("tracked.txt", "hello\n")
	for _, args := range [][]string{
		{"init", "-q", "."},
		{"config", "filter.evil.clean", hook},
		{"config", "user.email", "t@example.com"},
		{"config", "user.name", "t"},
		{"add", "-A"},
	} {
		if out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("setup %v: %v %s", args, err, out)
		}
	}
	// Break the stat cache the way extracting an archive does.
	old := filepath.Join(dir, "tracked.txt")
	if err := os.Chtimes(old, time.Unix(1, 0), time.Unix(1, 0)); err != nil {
		t.Fatal(err)
	}

	if !runs(t, dir, marker, func() {
		_ = exec.Command("git", "-C", dir, "status", "--porcelain", "-b").Run()
	}) {
		t.Skip("this git does not run the clean filter here; the test cannot prove anything")
	}
	if runs(t, dir, marker, func() { _ = In(dir, "status", "--porcelain", "-b").Run() }) {
		t.Error("hardened status ran the repository's clean filter")
	}
	if runs(t, dir, marker, func() {
		_ = In(dir, "diff", "--no-textconv", "--no-ext-diff").Run()
	}) {
		t.Error("hardened diff ran the repository's clean filter")
	}
}

// log.showSignature plus gpg.program is a second, independent path.
func TestSignatureVerificationCannotExecute(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	dir := t.TempDir()
	marker := filepath.Join(dir, "GPG_RAN")
	hook := filepath.Join(dir, "pwn.sh")
	if err := os.WriteFile(hook, []byte("#!/bin/sh\ntouch "+marker+"\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"init", "-q", "."},
		{"config", "user.email", "t@example.com"},
		{"config", "user.name", "t"},
		{"config", "log.showSignature", "true"},
		{"config", "gpg.program", hook},
		{"commit", "-q", "--allow-empty", "-m", "first"},
	} {
		if out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("setup %v: %v %s", args, err, out)
		}
	}
	if runs(t, dir, marker, func() {
		_ = In(dir, "log", "-1", "--format=%h %s").Run()
	}) {
		t.Error("hardened log ran the repository's gpg program")
	}
}
