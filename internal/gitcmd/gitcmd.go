// Package gitcmd builds git invocations that cannot be turned into code
// execution by the repository being inspected.
package gitcmd

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
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
// A fixed list of overrides is not sufficient, because the dangerous keys are
// not a fixed set: `filter.<anything>.clean` is chosen by the repository and
// selected by its own .gitattributes, and no override can pre-empt a name we
// have not seen. So the repository's local keys are read first — `git config
// --list` executes nothing — and every one that names a program is pinned to
// empty, where it outranks the file.
//
// The pins are delivered through GIT_CONFIG_COUNT / GIT_CONFIG_KEY_<n> /
// GIT_CONFIG_VALUE_<n>, not `git -c key=value`. `-c` splits its argument on the
// first '=', but a git config subsection name may legally contain '=': a repo
// with [filter "a=b"] selected by `filter=a=b` in .gitattributes enumerates as
// filter.a=b.clean, and `-c filter.a=b.clean=` parses as key "filter.a" with
// value "b.clean=", leaving the real clean command live during `git diff`. The
// environment form carries the key and the value in separate variables, so a
// key holding '=' (or any other byte) pins exactly the key it names.
type cfg struct{ key, val string }

var staticPins = []cfg{
	{"core.fsmonitor", "false"},
	{"core.hooksPath", "/dev/null"},
	{"core.pager", "cat"},
	{"core.sshCommand", "false"},
	{"core.editor", "false"},
	{"core.askPass", ""},
	{"diff.external", ""},
	{"log.showSignature", "false"},
	{"gpg.program", "false"},
	{"credential.helper", ""},
	{"protocol.ext.allow", "never"},
	{"uploadpack.packObjectsHook", ""},
	{"sequence.editor", "false"},
}

// staticFlags are real git options (not config), so they stay on the command
// line.
var staticFlags = []string{"--no-optional-locks"}

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
	pins      []cfg
}

var (
	pinMu      sync.Mutex
	pinsByRepo = map[string]pinCache{}
)

// repoPins reads the repository's local config and pins whatever in it names a
// program. Cached against the config file's size and mtime, since this runs on
// a timer.
func repoPins(dir string) []cfg {
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

func readPins(dir string) []cfg {
	// Every scope, not --local. --local reads one file: it does not expand
	// include.path, and it does not see .git/config.worktree when the repo
	// turns on extensions.worktreeConfig — so a filter defined in either was
	// invisible to the enumeration and fully live during status. Plain --list
	// reads all scopes and follows includes. It also reports the user's own
	// global keys, which are pinned too; that is the same neutering the static
	// list already applies to them.
	args := append([]string{"-C", dir}, staticFlags...)
	args = append(args, "config", "--list", "--name-only", "-z")
	cmd := exec.Command("git", args...)
	cmd.Env = configEnv(secretenv.Scrubbed(), staticPins)
	out, err := cmd.Output()
	if err != nil {
		return nil // not a repository, or no local config: nothing to pin
	}
	var pins []cfg
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
			pins = append(pins, cfg{key, ""})
		}
	}
	for name := range filters {
		pins = append(pins,
			cfg{"filter." + name + ".clean", ""},
			cfg{"filter." + name + ".smudge", ""},
			cfg{"filter." + name + ".process", ""},
			cfg{"filter." + name + ".required", "false"},
		)
	}
	return pins
}

// configEnv returns base with the pins appended as GIT_CONFIG_* variables. Any
// GIT_CONFIG_COUNT already present is honoured — the pins are numbered after
// the existing entries, so they take precedence over them (git reads higher
// indices last) without discarding config the user injected through the same
// mechanism. A malformed count is treated as zero and replaced.
func configEnv(base []string, pins []cfg) []string {
	if len(pins) == 0 {
		return base
	}
	start := 0
	out := make([]string, 0, len(base)+len(pins)*2+1)
	for _, kv := range base {
		if name, val, ok := strings.Cut(kv, "="); ok && name == "GIT_CONFIG_COUNT" {
			if n, err := strconv.Atoi(strings.TrimSpace(val)); err == nil && n > 0 {
				start = n
			}
			continue // re-emitted below with the new total
		}
		out = append(out, kv)
	}
	for i, p := range pins {
		n := strconv.Itoa(start + i)
		out = append(out, "GIT_CONFIG_KEY_"+n+"="+p.key, "GIT_CONFIG_VALUE_"+n+"="+p.val)
	}
	out = append(out, "GIT_CONFIG_COUNT="+strconv.Itoa(start+len(pins)))
	return out
}

func build(dir string, args []string) []string {
	full := append([]string{"-C", dir}, staticFlags...)
	return append(full, args...)
}

// env returns the scrubbed environment with every pin — static and repo-local
// — delivered as GIT_CONFIG_* variables.
func env(dir string) []string {
	pins := append(append([]cfg(nil), staticPins...), repoPins(dir)...)
	return configEnv(secretenv.Scrubbed(), pins)
}

// In returns a git command run inside dir. Callers add their subcommand and
// arguments; the safety flags are already in place.
func In(dir string, args ...string) *exec.Cmd {
	cmd := exec.Command("git", build(dir, args)...)
	cmd.Env = env(dir)
	return cmd
}

// InContext is In with a deadline, for calls that a hostile repository could
// otherwise stall.
func InContext(ctx context.Context, dir string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "git", build(dir, args)...)
	cmd.Env = env(dir)
	return cmd
}
