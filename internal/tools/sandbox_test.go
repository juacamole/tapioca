package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// runIn executes a bash command through the executor and returns its output.
func runIn(t *testing.T, e *Executor, command string) (string, bool) {
	t.Helper()
	raw, _ := json.Marshal(map[string]string{"command": command})
	out, isErr, err := e.Call(context.Background(), "bash", raw, allow)
	if err != nil {
		t.Fatal(err)
	}
	return out, isErr
}

func sandboxExecutor(t *testing.T, work string) *Executor {
	t.Helper()
	if !SandboxAvailable() {
		t.Skip("bubblewrap not installed")
	}
	e := NewExecutor(work, ModeBypass)
	e.SetSandbox(true)
	e.SetSandboxNetwork(true)
	return e
}

func TestSandboxHidesHomeButKeepsTheWorktreeWritable(t *testing.T) {
	work := t.TempDir()
	e := sandboxExecutor(t, work)

	// Sanity: the sandbox must still be able to run ordinary commands.
	if out, isErr := runIn(t, e, "echo hello"); isErr || !strings.Contains(out, "hello") {
		t.Fatalf("plain command failed inside the sandbox: %q", out)
	}

	// The worktree is writable, and the write lands on the real filesystem.
	if out, isErr := runIn(t, e, "echo written > proof.txt"); isErr {
		t.Fatalf("writing in the worktree failed: %q", out)
	}
	data, err := os.ReadFile(filepath.Join(work, "proof.txt"))
	if err != nil || strings.TrimSpace(string(data)) != "written" {
		t.Fatalf("worktree write did not reach the host: %v %q", err, data)
	}

	// A secret in the real $HOME must not be visible.
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home directory")
	}
	secret := filepath.Join(home, ".tapioca-sandbox-probe")
	if err := os.WriteFile(secret, []byte("TOP-SECRET-VALUE"), 0o600); err != nil {
		t.Skip("cannot write a probe into $HOME")
	}
	defer os.Remove(secret)

	out, _ := runIn(t, e, "cat ~/.tapioca-sandbox-probe 2>&1")
	if strings.Contains(out, "TOP-SECRET-VALUE") {
		t.Fatalf("sandbox exposed a file in $HOME: %q", out)
	}
}

func TestSandboxBlocksWritesOutsideTheWorktree(t *testing.T) {
	work := t.TempDir()
	outside := t.TempDir()
	e := sandboxExecutor(t, work)

	target := filepath.Join(outside, "victim.txt")
	if err := os.WriteFile(target, []byte("original"), 0o644); err != nil {
		t.Fatal(err)
	}
	runIn(t, e, "echo clobbered > "+target+" 2>&1")

	data, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(data)) != "original" {
		t.Fatalf("sandbox allowed a write outside the worktree: %q", data)
	}
}

func TestSandboxNetworkToggle(t *testing.T) {
	work := t.TempDir()
	e := sandboxExecutor(t, work)

	e.SetSandboxNetwork(false)
	args := e.sandboxArgs("true")
	if !contains(args, "--unshare-net") {
		t.Error("network denial did not add --unshare-net")
	}
	e.SetSandboxNetwork(true)
	if contains(e.sandboxArgs("true"), "--unshare-net") {
		t.Error("network was unshared even though it is allowed")
	}
}

func TestSandboxRefusesWithoutBwrap(t *testing.T) {
	// Simulate a machine without bubblewrap by pointing the lookup at nothing.
	e := NewExecutor(t.TempDir(), ModeBypass)
	e.SetSandbox(true)
	t.Setenv("PATH", t.TempDir())
	bwrapOnce = onceReset()
	defer func() { bwrapOnce = onceReset(); bwrapPath = "" }()

	out, isErr := runIn(t, e, "echo should-not-run")
	if !isErr || !strings.Contains(out, "bubblewrap") {
		t.Fatalf("missing bwrap should refuse loudly, got %q (isErr=%v)", out, isErr)
	}
	if strings.Contains(out, "should-not-run") {
		t.Fatal("the command ran unsandboxed after the sandbox was requested")
	}
}

func onceReset() sync.Once { return sync.Once{} }

func contains(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}
