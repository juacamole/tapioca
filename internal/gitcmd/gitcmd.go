// Package gitcmd builds git invocations that cannot be turned into code
// execution by the repository being inspected.
package gitcmd

import (
	"context"
	"crypto/sha256"
	"io"
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
		".clean", ".smudge", ".process", ".command", ".cmd", ".textconv", ".driver",
		".program", ".helper", ".hook", ".uploadpack", ".receivepack", ".askpass",
	} {
		// With the dot and without it. git spells the same idea both ways: the
		// sectioned form the dot was written for (diff.myconv.textconv), and a
		// compound word in one component — core.sshCommand, core.askPass,
		// uploadpack.packObjectsHook. Each of those is in the exact list above,
		// which is how it was noticed that core.alternateRefsCommand is not:
		// ".command" does not end "…refscommand", so the one key nobody thought
		// of was the one spelling the rule could not reach. Matching the word
		// rather than the component covers both spellings, including the next
		// compound.
		if strings.HasSuffix(k, suffix) || strings.HasSuffix(k, suffix[1:]) {
			return true
		}
	}
	return false
}

// pinCache holds one repository's pins together with every file git said the
// configuration came from, and what was in each of them.
//
// Keying this on .git/config's size and mtime was not sound, because the keys
// git obeys do not all live in that file. An include.path names another one,
// and the repository chooses where it sits — including inside the worktree,
// where an ordinary in-tree edit reaches it and looks nothing like a change to
// git's configuration. A filter driver written there left .git/config's stat
// untouched, so the cached pins still described the config as it was, and the
// new filter.<name>.clean was live during the next status poll: a file write
// the user approved for what it appeared to be turned into arbitrary execution
// seconds later.
//
// Contents rather than size and mtime, because both of those are things a
// writer can hold still — `touch -r` restores an mtime to the nanosecond, and
// a config file can be edited without changing its length.
type pinCache struct {
	files []cfgFile
	pins  []cfg
}

// cfgFile is one file the configuration was read from, and its contents.
type cfgFile struct {
	path string
	sum  [sha256.Size]byte
}

var (
	pinMu      sync.Mutex
	pinsByRepo = map[string]pinCache{}
)

// repoPins reads the repository's config and pins whatever in it names a
// program. Cached against the contents of every file that config came from,
// since this runs on a timer.
func repoPins(dir string) []cfg {
	pinMu.Lock()
	c, ok := pinsByRepo[dir]
	pinMu.Unlock()
	if ok && current(c.files) {
		return c.pins
	}
	files, pins := readPins(dir)
	pinMu.Lock()
	pinsByRepo[dir] = pinCache{files: files, pins: pins}
	pinMu.Unlock()
	return pins
}

// current reports whether every file the pins were read from is still exactly
// as it was. An empty list is never current: that is what readPins returns
// when it could not establish where the configuration came from, and reusing
// pins of unknown provenance is the hole this cache had. Costing a `git config
// --list` per command is the right answer to not knowing.
func current(files []cfgFile) bool {
	if len(files) == 0 {
		return false
	}
	for _, f := range files {
		if sum, ok := digest(f.path); !ok || sum != f.sum {
			return false
		}
	}
	return true
}

// maxConfigBytes bounds what is re-read to check the cache. Real config files
// are a few kilobytes; one larger than this is reported as unreadable, which
// only means its repository's pins are re-read rather than cached.
const maxConfigBytes = 1 << 20

func digest(path string) (sum [sha256.Size]byte, ok bool) {
	f, err := os.Open(path)
	if err != nil {
		return sum, false
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, io.LimitReader(f, maxConfigBytes+1))
	if err != nil || n > maxConfigBytes {
		return sum, false
	}
	copy(sum[:], h.Sum(nil))
	return sum, true
}

// configOrigins turns the origins `git config --show-origin` reported into the
// absolute paths of files that can be watched. git writes a path relative to
// the top of the worktree when the file is inside it — ".git/config" is the
// ordinary case — so the top has to be asked for. Anything else that is not a
// plain file (a blob in the object store, standard input) has no contents on
// disk to compare against, and one of those makes the whole set unusable
// rather than partly checked.
//
// "command line:" is the exception, and it is this process's own doing: the
// static pins are handed to git through GIT_CONFIG_* and come back attributed
// there. Those, and any GIT_CONFIG_* the user exported before Tapioca started,
// are fixed for the life of the process, so there is nothing about them to
// watch. Refusing them would make the whole set unusable every time and reduce
// the cache to nothing.
func configOrigins(dir string, origins map[string]bool) []cfgFile {
	top := ""
	var files []cfgFile
	for o := range origins {
		if o == "command line:" {
			continue
		}
		path, isFile := strings.CutPrefix(o, "file:")
		if !isFile {
			return nil
		}
		if !filepath.IsAbs(path) {
			if top == "" {
				// The same environment every other git child gets: the pins in
				// place, and no provider keys handed to a subprocess.
				cmd := exec.Command("git", append(append([]string{"-C", dir}, staticFlags...), "rev-parse", "--show-toplevel")...)
				cmd.Env = configEnv(secretenv.Scrubbed(), staticPins)
				out, err := cmd.Output()
				if top = strings.TrimSpace(string(out)); err != nil || top == "" {
					return nil
				}
			}
			path = filepath.Join(top, path)
		}
		sum, ok := digest(path)
		if !ok {
			return nil
		}
		files = append(files, cfgFile{path: path, sum: sum})
	}
	return files
}

// namesAnotherFile reports whether a key means "git also reads a file this
// enumeration cannot see the effect of yet".
//
// --show-origin names the file each *key* came from, so a file that produced no
// key is named by nothing — and a file that produced no key today can produce
// one tomorrow without any watched file changing:
//
//   - include.path may name a file that does not exist. git ignores a missing
//     include in silence, so it is absent from the origins, and creating it
//     later is an ordinary in-tree write that auto mode approves without
//     asking. The filter driver written into it is live on the next status
//     poll while the cache still answers with the pins read before it existed.
//   - the same holds for an include target that exists but holds only
//     comments, which is less conspicuous to ship than a dangling include.
//   - includeIf conditions are evaluated per command and reported as keys even
//     while false. `[includeIf "onbranch:x"]` starts reading its file the
//     moment the branch is checked out, and a checkout writes .git/HEAD, not
//     .git/config.
//   - extensions.worktreeConfig makes git read $GIT_DIR/config.worktree, which
//     need not exist yet either.
//
// Resolving the include targets and watching them for appearance was the other
// way out, and it needs the values, the directory each include is relative to,
// and a reading of every condition git might newly satisfy — three more things
// to be wrong about. Not caching costs one `git config --list` per git command
// in a repository that uses includes, and git is run on a five-second timer.
func namesAnotherFile(key string) bool {
	k := strings.ToLower(key)
	if k == "extensions.worktreeconfig" {
		return true
	}
	if k == "include.path" {
		return true
	}
	return strings.HasPrefix(k, "includeif.") && strings.HasSuffix(k, ".path")
}

// originPrefixes are how `git config --show-origin` labels where a value came
// from. A config key never starts with one: its first component is a section
// name, and a section name holds no ':'.
var originPrefixes = []string{"file:", "blob:", "submodule-blob:", "command line:", "standard input:"}

func isOrigin(field string) bool {
	for _, p := range originPrefixes {
		if strings.HasPrefix(field, p) {
			return true
		}
	}
	return false
}

// listConfig enumerates every config key that applies in dir, with the file
// each came from.
//
// The fallback is not decoration. --show-origin is what tells the cache which
// files to watch, and it is also a flag a git old enough not to have it would
// reject — turning the enumeration into an error, and an error here into no
// repository pins at all. Losing the origins costs a re-read per command;
// losing the pins is the hole. So the second attempt drops the flag and keeps
// the enumeration, and with no origins reported nothing is ever cached.
func listConfig(dir string) ([]byte, bool) {
	base := append(append([]string{"-C", dir}, staticFlags...), "config", "--list", "--name-only", "-z")
	for _, args := range [][]string{append(base, "--show-origin"), base} {
		cmd := exec.Command("git", args...)
		cmd.Env = configEnv(secretenv.Scrubbed(), staticPins)
		if out, err := cmd.Output(); err == nil {
			return out, true
		}
	}
	return nil, false
}

// readPins returns the files the configuration was read from and the pins that
// neutralise it.
func readPins(dir string) ([]cfgFile, []cfg) {
	// Every scope, not --local. --local reads one file: it does not expand
	// include.path, and it does not see .git/config.worktree when the repo
	// turns on extensions.worktreeConfig — so a filter defined in either was
	// invisible to the enumeration and fully live during status. Plain --list
	// reads all scopes and follows includes. It also reports the user's own
	// global keys, which are pinned too; that is the same neutering the static
	// list already applies to them.
	//
	// --show-origin names the file each key came from, which is what the cache
	// above watches: the set of files is not knowable without asking git, since
	// an include may name any path at all.
	out, ok := listConfig(dir)
	if !ok {
		return nil, nil // not a repository, or no local config: nothing to pin
	}
	var pins []cfg
	filters := map[string]bool{}
	origins := map[string]bool{}
	unknowable := false
	// The records alternate origin, key. They are told apart by what they look
	// like rather than by their position: an origin begins with one of git's
	// own prefixes, and a config key cannot, because its first component is a
	// section name and a section name holds no ':'. Counting positions instead
	// would mean that a git whose output is shaped even slightly differently
	// fed origins to namesProgram and never saw a key — pins silently empty,
	// which is the failure this whole file exists to prevent.
	for _, field := range strings.Split(string(out), "\x00") {
		if field == "" {
			continue
		}
		if isOrigin(field) {
			origins[field] = true
			continue
		}
		key := field
		if namesAnotherFile(key) {
			unknowable = true
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
	if unknowable {
		// The pins are right for the configuration as it stands; it is only
		// their reuse that is unsound, and an empty file list is how this says
		// "re-read me". The pins themselves are still returned, because
		// returning none is the hole the whole file exists to prevent.
		return nil, pins
	}
	return configOrigins(dir, origins), pins
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

// Pin is one config override for a git run built outside this package.
type Pin struct{ Key, Value string }

// WithPins returns base with pins delivered as GIT_CONFIG_* variables, which
// is the same mechanism env() uses and for the same reason: `-c` cannot carry
// a key containing '='. A caller that builds its own git invocation — the
// checkpoint repo, which sets GIT_DIR itself — still has to neutralise the
// keys that name programs.
func WithPins(base []string, pins ...Pin) []string {
	cs := make([]cfg, 0, len(pins))
	for _, p := range pins {
		cs = append(cs, cfg{p.Key, p.Value})
	}
	return configEnv(base, cs)
}

// StaticPins is the list every git run inside this package neutralises,
// exported so a caller that builds its own invocation answers the question the
// same way. Two places deciding which keys name a program, and disagreeing
// about it, is how the shorter list ends up being the one that runs.
func StaticPins() []Pin {
	out := make([]Pin, 0, len(staticPins))
	for _, c := range staticPins {
		out = append(out, Pin{Key: c.key, Value: c.val})
	}
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
