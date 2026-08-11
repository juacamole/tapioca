// Package gitcmd builds git invocations that cannot be turned into code
// execution by the repository being inspected.
package gitcmd

import (
	"context"
	"os/exec"

	"tapioca/internal/secretenv"
)

// A repository's own .git/config is data written by whoever produced the
// directory, and several of its keys name programs git will run. core.fsmonitor
// is the sharpest: `git status` executes it, and Tapioca polls status every few
// seconds for the git panel — so merely opening a directory, in any permission
// mode including plan, ran whatever that directory chose, before the user had
// typed anything. An extracted tarball or a copied tree is enough; `git clone`
// is not, since clone writes a fresh config.
//
// -c on the command line outranks the repository's config, so every key that
// names a program is pinned here. --no-optional-locks keeps a read from
// writing to the repository as a side effect.
var hardening = []string{
	"-c", "core.fsmonitor=false",
	"-c", "core.hooksPath=/dev/null",
	"-c", "core.pager=cat",
	"-c", "core.sshCommand=false",
	"-c", "core.editor=false",
	"-c", "diff.external=",
	"-c", "protocol.ext.allow=never",
	"-c", "uploadpack.packObjectsHook=",
	"--no-optional-locks",
}

// In returns a git command run inside dir. Callers add their subcommand and
// arguments; the safety flags are already in place.
func In(dir string, args ...string) *exec.Cmd {
	full := append([]string{"-C", dir}, hardening...)
	cmd := exec.Command("git", append(full, args...)...)
	cmd.Env = secretenv.Scrubbed()
	return cmd
}

// InContext is In with a deadline, for calls that a hostile repository could
// otherwise stall.
func InContext(ctx context.Context, dir string, args ...string) *exec.Cmd {
	full := append([]string{"-C", dir}, hardening...)
	cmd := exec.CommandContext(ctx, "git", append(full, args...)...)
	cmd.Env = secretenv.Scrubbed()
	return cmd
}
