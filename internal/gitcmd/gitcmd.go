// Package gitcmd builds git invocations that cannot be turned into code
// execution by the repository being inspected.
package gitcmd

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"

	"tapioca/internal/secretenv"
)

// A repository's own .git/config is data written by whoever produced the
// directory, and many of its keys name programs git will run. core.fsmonitor
// is the sharpest — `git status` executes it, and Tapioca polls status every
// few seconds for the git panel — so merely opening a directory, in any
// permission mode including plan, ran whatever that directory chose, before
// the user had typed anything. An extracted tarball or a copied tree is
// enough; `git clone` is not, since clone writes a fresh config.
//
// A fixed list of -c overrides is not sufficient, because the dangerous keys
// are not a fixed set: `filter.<anything>.clean` is chosen by the repository
// and selected by its own .gitattributes, and no -c can pre-empt a name we
// have not seen. So the repository's local keys are read first — `git config
// --list` executes nothing — and every one that names a program is pinned to
// empty on the command line, where it outranks the file.
var staticPins = []string{
	"-c", "core.fsmonitor=false",
	"-c", "core.hooksPath=/dev/null",
	"-c", "core.pager=cat",
	"-c", "core.sshCommand=false",
	"-c", "core.editor=false",
	"-c", "core.askPass=",
	"-c", "diff.external=",
	"-c", "log.showSignature=false",
	"-c", "gpg.program=false",
	"-c", "credential.helper=",
	"-c", "protocol.ext.allow=never",
	"-c", "uploadpack.packObjectsHook=",
	"-c", "sequence.editor=false",
	"--no-optional-locks",
}

// namesProgram reports whether a config key's value is something git executes.
// Suffix matching covers the sectioned forms, where the middle component is a
// name the repository invents: gpg.ssh.program, diff.myconv.textconv.
func namesProgram(key string) bool {
	k := strings.ToLower(key)
	switch k {
	case "core.fsmonitor", "core.hookspath", "core.sshcommand", "core.editor",
		"core.askpass", "core.pager", "core.gitproxy", "diff.external",
		"log.showsignature", "sequence.editor", "init.templatedir",
		"uploadpack.packobjectshook", "credential.helper":
		return true
	}
	for _, suffix := range []string{
		".clean", ".smudge", ".process", ".command", ".textconv", ".driver",
		".program", ".helper", ".hook", ".uploadpack", ".receivepack", ".askpass",
	} {
		if strings.HasSuffix(k, suffix) {
			return true
		}
	}
	return false
}

type pinCache struct {
	size, mod int64
	pins      []string
}

var (
	pinMu      sync.Mutex
	pinsByRepo = map[string]pinCache{}
)

// repoPins reads the repository's local config and pins whatever in it names a
// program. Cached against the config file's size and mtime, since this runs on
// a timer.
func repoPins(dir string) []string {
	path := filepath.Join(dir, ".git", "config")
	var size, mod int64
	if info, err := os.Stat(path); err == nil {
		size, mod = info.Size(), info.ModTime().UnixNano()
	}
	pinMu.Lock()
	if c, ok := pinsByRepo[dir]; ok && c.size == size && c.mod == mod {
		pinMu.Unlock()
		return c.pins
	}
	pinMu.Unlock()

	pins := readPins(dir)
	pinMu.Lock()
	pinsByRepo[dir] = pinCache{size: size, mod: mod, pins: pins}
	pinMu.Unlock()
	return pins
}

func readPins(dir string) []string {
	// Every scope, not --local. --local reads one file: it does not expand
	// include.path, and it does not see .git/config.worktree when the repo
	// turns on extensions.worktreeConfig — so a filter defined in either was
	// invisible to the enumeration and fully live during status. Plain --list
	// reads all scopes and follows includes. It also reports the user's own
	// global keys, which are pinned too; that is the same neutering the static
	// list already applies to them.
	args := append([]string{"-C", dir}, staticPins...)
	args = append(args, "config", "--list", "--name-only", "-z")
	cmd := exec.Command("git", args...)
	cmd.Env = secretenv.Scrubbed()
	out, err := cmd.Output()
	if err != nil {
		return nil // not a repository, or no local config: nothing to pin
	}
	var pins []string
	filters := map[string]bool{}
	for _, key := range strings.Split(string(out), "\x00") {
		if key == "" {
			continue
		}
		// A filter driver is pinned as a whole: an empty clean command with
		// required=true would make git refuse to read the file at all.
		if strings.HasPrefix(strings.ToLower(key), "filter.") {
			if parts := strings.Split(key, "."); len(parts) >= 3 {
				filters[strings.Join(parts[1:len(parts)-1], ".")] = true
			}
			continue
		}
		if namesProgram(key) {
			pins = append(pins, "-c", key+"=")
		}
	}
	for name := range filters {
		pins = append(pins,
			"-c", "filter."+name+".clean=",
			"-c", "filter."+name+".smudge=",
			"-c", "filter."+name+".process=",
			"-c", "filter."+name+".required=false",
		)
	}
	return pins
}

func build(dir string, args []string) []string {
	full := append([]string{"-C", dir}, staticPins...)
	full = append(full, repoPins(dir)...)
	return append(full, args...)
}

// In returns a git command run inside dir. Callers add their subcommand and
// arguments; the safety flags are already in place.
func In(dir string, args ...string) *exec.Cmd {
	cmd := exec.Command("git", build(dir, args)...)
	cmd.Env = secretenv.Scrubbed()
	return cmd
}

// InContext is In with a deadline, for calls that a hostile repository could
// otherwise stall.
func InContext(ctx context.Context, dir string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "git", build(dir, args)...)
	cmd.Env = secretenv.Scrubbed()
	return cmd
}
