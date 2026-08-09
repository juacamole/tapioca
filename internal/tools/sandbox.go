package tools

import (
	"os"
	"os/exec"
	"path/filepath"
	"sync"
)

// Sandboxing is containment rather than filtering: the permission gate decides
// whether a command runs, and this decides what it can reach if it does. It
// covers bash only — the file tools are Go code inside the process and are
// already confined by their own checks.

var (
	bwrapOnce sync.Once
	bwrapPath string
)

// bwrap locates bubblewrap, which must be setuid or user-namespace capable.
func bwrap() string {
	bwrapOnce.Do(func() {
		bwrapPath, _ = exec.LookPath("bwrap")
	})
	return bwrapPath
}

// SandboxAvailable reports whether sandboxing can be used on this machine.
func SandboxAvailable() bool { return bwrap() != "" }

// SetSandbox turns bash sandboxing on or off.
func (e *Executor) SetSandbox(on bool) {
	e.mu.Lock()
	e.sandbox = on
	e.mu.Unlock()
}

// Sandboxed reports whether bash commands are confined.
func (e *Executor) Sandboxed() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.sandbox
}

// SetSandboxNetwork allows or denies network access inside the sandbox.
func (e *Executor) SetSandboxNetwork(allow bool) {
	e.mu.Lock()
	e.sandboxNet = allow
	e.mu.Unlock()
}

// sandboxArgs builds the bwrap invocation for a shell command: the whole
// filesystem readable so tools work, $HOME replaced by an empty tmpfs so keys
// and browser data are simply not there, and the working tree bound writable.
func (e *Executor) sandboxArgs(command string) []string {
	cwd := e.Cwd()
	home, err := os.UserHomeDir()
	if err != nil {
		home = ""
	}

	args := []string{
		"--ro-bind", "/", "/",
		"--dev", "/dev",
		"--proc", "/proc",
		"--tmpfs", "/tmp",
		"--die-with-parent",
		"--unshare-pid", "--unshare-ipc", "--unshare-uts",
	}
	if home != "" {
		// Everything under $HOME disappears, then only what the agent is
		// meant to touch is bound back over it.
		args = append(args, "--tmpfs", home)
		// git refuses to commit without an identity, and it lives here.
		for _, keep := range []string{".gitconfig", ".config/git"} {
			p := filepath.Join(home, keep)
			if _, err := os.Stat(p); err == nil {
				args = append(args, "--ro-bind", p, p)
			}
		}
	}
	args = append(args, "--bind", cwd, cwd)
	for _, d := range e.ExtraDirs() {
		if abs, err := filepath.Abs(d); err == nil {
			if _, err := os.Stat(abs); err == nil {
				args = append(args, "--bind", abs, abs)
			}
		}
	}
	if home != "" {
		args = append(args, "--setenv", "HOME", home)
	}
	args = append(args, "--chdir", cwd)

	e.mu.Lock()
	allowNet := e.sandboxNet
	e.mu.Unlock()
	if !allowNet {
		args = append(args, "--unshare-net")
	}
	return append(args, "--", "sh", "-c", command)
}
