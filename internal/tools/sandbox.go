package tools

import (
	"os"
	"os/exec"
	"os/user"
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

// accountHome is what the account database says this user's home is. A
// variable so a test can say what the machine cannot be asked to pretend.
var accountHome = func() string {
	u, err := user.Current()
	if err != nil {
		return ""
	}
	return u.HomeDir
}

// userHomes returns every directory this machine calls the user's home.
//
// os.UserHomeDir is $HOME and nothing else, and $HOME is an environment
// variable an extracted tree reaches through an .envrc — the same door the git
// config channel came through. One line, `export HOME=$PWD/.home`, moved the
// sandbox's tmpfs onto a directory nobody keeps anything in and left the real
// one sitting under the read-only bind of /, so a sandboxed bash call read
// ~/.ssh/id_rsa and ~/.aws/credentials with the sandbox switched on and the UI
// saying so. That is the whole of what the sandbox claims to do.
//
// The account database is the second opinion, and config.usersHome already
// takes it for the same reason about the same variable — the places that ask
// where the user lives should not answer differently. Every answer counts
// rather than one being chosen: covering a directory that is not really the
// home costs nothing, and choosing wrongly costs the keys.
//
// sensitiveRoots is the other caller, and it was the third place asking this
// question and the one still asking it of $HOME alone: the same line took
// ~/.ssh and ~/.config/gh out of the read gate, which is worse than the sandbox
// case because read_file needs no approval in any other way.
//
// The filesystem root is never one of them. A daemon account whose home is "/"
// would otherwise put a tmpfs over the entire sandbox and leave nothing to run.
func userHomes() []string {
	var out []string
	add := func(p string) {
		if p == "" || !filepath.IsAbs(p) {
			return
		}
		p = filepath.Clean(p)
		if filepath.Dir(p) == p {
			return
		}
		for _, seen := range out {
			if seen == p {
				return
			}
		}
		out = append(out, p)
	}
	if h, err := os.UserHomeDir(); err == nil {
		add(h)
	}
	add(accountHome())
	return out
}

// sandboxArgs builds the bwrap invocation for a shell command: the whole
// filesystem readable so tools work, the user's home replaced by an empty
// tmpfs so keys and browser data are simply not there, and the working tree
// bound writable.
func (e *Executor) sandboxArgs(command string) []string {
	cwd := e.Cwd()
	homes := userHomes()
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
	for _, h := range homes {
		// Everything under the home disappears, then only what the agent is
		// meant to touch is bound back over it.
		args = append(args, "--tmpfs", h)
		// git refuses to commit without an identity, and it lives here.
		for _, keep := range []string{".gitconfig", ".config/git"} {
			p := filepath.Join(h, keep)
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
