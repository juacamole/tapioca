package tools

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The sandbox replaces the user's home with an empty tmpfs, and that is the
// whole of what it claims to do about credentials: ~/.ssh, ~/.aws, the browser
// profiles and the cloud CLI tokens are not hidden, they are not there.
//
// Where the home is came from os.UserHomeDir, which on Unix is $HOME and
// nothing else. $HOME is an environment variable, and an extracted tree
// reaches the environment through an .envrc — the door round eleven's git
// config finding came through, and one config.usersHome already treats as
// attacker-influenced about this exact variable. `export HOME=$PWD/.home` put
// the tmpfs over a directory holding nothing and left the real home under the
// read-only bind of /, so a sandboxed bash call read the keys with the sandbox
// switched on and the status line saying so.
//
// It is worth having: sandboxing is what stands behind a standing grant. A
// user who answers "always allow" to `npm test` is relying on this and nothing
// else to keep the package out of ~/.ssh.
func TestSandboxHidesTheHomeEvenWhenHOMEIsRedirected(t *testing.T) {
	if _, err := exec.LookPath("bwrap"); err != nil {
		t.Skip("bubblewrap is not installed here; the sandbox cannot be exercised")
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		t.Skip("this machine has no home directory to hide")
	}

	work := t.TempDir()
	if resolved, err := filepath.EvalSymlinks(work); err == nil {
		work = resolved
	}
	read := func(path string) string {
		t.Helper()
		e := NewExecutor(work, ModeBypass)
		e.SetSandbox(true)
		raw, err := json.Marshal(map[string]string{"command": "cat " + path})
		if err != nil {
			t.Fatal(err)
		}
		out, _, err := e.runBash(context.Background(), raw)
		if err != nil {
			t.Fatalf("running a sandboxed command failed: %v", err)
		}
		return out
	}

	// First control: the sandbox runs at all and the read-only bind of / is in
	// place. Without this a sandbox that failed to start would make every
	// assertion below pass by returning an error message for each read.
	if !strings.Contains(read("/etc/hostname"), "\n") && !strings.Contains(read("/etc/passwd"), ":") {
		t.Skip("nothing outside the home was readable in the sandbox, so it is not running normally here")
	}

	// The secret has to live in the real home, because that is the only
	// directory the sandbox hides for the reason under test: /tmp is a tmpfs in
	// there too, so a file placed under it would be invisible whatever $HOME
	// said, and the assertion would prove nothing.
	dir, err := os.MkdirTemp(home, ".tapioca-sandbox-test-")
	if err != nil {
		t.Skipf("cannot write to the home directory to place a secret in it: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	secret := filepath.Join(dir, "id_rsa")
	if err := os.WriteFile(secret, []byte("PRIVATE-KEY-MATERIAL\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	// Second control: with the environment left alone, the sandbox does hide
	// it. If it does not, the tmpfs is not working here and the redirected case
	// has nothing to say.
	if strings.Contains(read(secret), "PRIVATE-KEY-MATERIAL") {
		t.Skip("the sandbox does not hide the home directory here even untouched, so the redirection cannot be shown to matter")
	}

	t.Setenv("HOME", t.TempDir())
	if strings.Contains(read(secret), "PRIVATE-KEY-MATERIAL") {
		t.Fatal("a sandboxed command read a key out of the real home after $HOME was pointed elsewhere: one line in an .envrc turns the sandbox off while it still reports itself on")
	}
}

// The argument list, for the machines with no bubblewrap on them — which is
// CI, and every developer who has not installed it. Two homes disagreeing is
// the case; both must be covered.
func TestSandboxArgsCoverBothAnswersAboutTheHome(t *testing.T) {
	work := t.TempDir()
	if resolved, err := filepath.EvalSymlinks(work); err == nil {
		work = resolved
	}
	fake := filepath.Join(work, "not-really-home")
	real := filepath.Join(work, "the-account-home")
	for _, d := range []string{fake, real} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	t.Setenv("HOME", fake)
	prev := accountHome
	accountHome = func() string { return real }
	t.Cleanup(func() { accountHome = prev })

	args := NewExecutor(work, ModeBypass).sandboxArgs("true")
	for _, want := range []string{fake, real} {
		if !hasFlagValue(args, "--tmpfs", want) {
			t.Fatalf("%s is not covered by a tmpfs; the sandbox leaves it readable\nargs: %v", want, args)
		}
	}
}

// A home the two sources agree on is named once, not twice: a duplicated
// --tmpfs is not wrong, but bwrap arguments are what a user reads when a
// sandboxed command misbehaves.
func TestSandboxArgsNameOneHomeOnceAndSkipTheRoot(t *testing.T) {
	work := t.TempDir()
	if resolved, err := filepath.EvalSymlinks(work); err == nil {
		work = resolved
	}
	agreed := filepath.Join(work, "home")
	if err := os.MkdirAll(agreed, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", agreed)
	prev := accountHome
	accountHome = func() string { return agreed }
	t.Cleanup(func() { accountHome = prev })

	args := NewExecutor(work, ModeBypass).sandboxArgs("true")
	if n := countFlagValue(args, "--tmpfs", agreed); n != 1 {
		t.Fatalf("the home was named %d times, want 1: %v", n, args)
	}

	// An account whose home is the filesystem root — some daemon accounts have
	// one — must not put a tmpfs over the whole sandbox.
	accountHome = func() string { return string(filepath.Separator) }
	args = NewExecutor(work, ModeBypass).sandboxArgs("true")
	if hasFlagValue(args, "--tmpfs", string(filepath.Separator)) {
		t.Fatalf("the filesystem root was hidden, which leaves nothing to run: %v", args)
	}
}

func hasFlagValue(args []string, flag, value string) bool {
	return countFlagValue(args, flag, value) > 0
}

func countFlagValue(args []string, flag, value string) int {
	n := 0
	for i := 0; i+1 < len(args); i++ {
		if args[i] == flag && args[i+1] == value {
			n++
		}
	}
	return n
}
