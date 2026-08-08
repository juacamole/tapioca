// Package tools implements Tapioca's built-in coding tools (shell, file
// read/write/edit) with a permission gate, making agents able to work on a
// codebase without any MCP server.
package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

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
	maxOutput   = 30_000
	execTimeout = 3 * time.Minute
)

// Executor runs built-in tools inside a working directory.
type Executor struct {
	mu           sync.Mutex
	cwd          string
	mode         string
	allowed      map[string]bool
	bashPrefixes []string
	extraDirs    []string
	checkpoint   func(label string)
}

// SetBashPrefixes replaces the always-allowed bash command prefixes.
func (e *Executor) SetBashPrefixes(ps []string) {
	e.mu.Lock()
	e.bashPrefixes = append([]string(nil), ps...)
	e.mu.Unlock()
}

var segSeps = regexp.MustCompile(`&&|\|\||[;|\n]`)

// segments splits a compound command so each part can be approved on its
// own: "ls || pwd" prompts for "ls", then for "pwd".
func segments(command string) []string {
	var out []string
	for _, seg := range segSeps.Split(command, -1) {
		if seg = strings.TrimSpace(seg); seg != "" {
			out = append(out, seg)
		}
	}
	return out
}

func (e *Executor) wordAllowed(word string) bool {
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
		strings.Contains(segment, "${") || strings.Contains(segment, "<(") {
		return true
	}
	inSingle, inDouble := false, false
	for i := 0; i < len(segment); i++ {
		switch c := segment[i]; c {
		case '\\':
			i++
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
	if escapes(segment) {
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

// interpreters execute arbitrary code from arguments or stdin, so granting
// them is equivalent to granting everything; the UI declines to offer it.
// Exec-wrappers (env, timeout, sudo, …) count: they run their arguments.
var interpreters = map[string]bool{
	"sh": true, "bash": true, "zsh": true, "fish": true, "dash": true,
	"ksh": true, "csh": true, "tcsh": true, "pwsh": true, "nu": true,
	"python": true, "python3": true, "perl": true, "ruby": true, "node": true,
	"deno": true, "bun": true, "php": true, "lua": true, "tclsh": true,
	"awk": true, "gawk": true, "mawk": true, "sed": true,
	"env": true, "eval": true, "exec": true, "command": true, "builtin": true,
	"xargs": true, "nohup": true, "setsid": true, "timeout": true, "time": true,
	"nice": true, "ionice": true, "stdbuf": true, "watch": true,
	"sudo": true, "doas": true, "su": true, "ssh": true, "chroot": true,
	"nix-shell": true, "nix": true, "docker": true, "podman": true,
}

// isInterpreter matches path and version variants too: /usr/bin/python3,
// python3.11 and bash-5.2 are the same grant as their bare name.
func isInterpreter(word string) bool {
	base := strings.ToLower(filepath.Base(word))
	if interpreters[base] {
		return true
	}
	trimmed := strings.TrimRight(base, "0123456789.-")
	return trimmed != base && interpreters[trimmed]
}

// PrefixSuggestion returns the word an always-allow-prefix grant would use,
// or "" when a blanket grant for it would be meaningless or unsafe.
func PrefixSuggestion(command string) string {
	f := strings.Fields(command)
	if len(f) == 0 {
		return ""
	}
	return f[0]
}

// PrefixGrantable reports whether [p] should be offered for a segment.
func PrefixGrantable(segment string) bool {
	w := PrefixSuggestion(segment)
	return w != "" && !isInterpreter(w) && !escapes(segment)
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

var toolNames = []string{"bash", "read_file", "write_file", "edit_file", "web_search", "web_fetch"}

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
			InputSchema: json.RawMessage(`{"type":"object","properties":{"command":{"type":"string","description":"The shell command to run"}},"required":["command"]}`),
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

func (e *Executor) resolve(path string) string {
	if strings.HasPrefix(path, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			path = filepath.Join(home, path[2:])
		}
	}
	if !filepath.IsAbs(path) {
		path = filepath.Join(e.Cwd(), path)
	}
	return filepath.Clean(path)
}

// Call executes a built-in tool. It returns (result, isError, err); err is
// reserved for internal failures, tool-level problems are (msg, true, nil).
func (e *Executor) Call(ctx context.Context, name string, raw json.RawMessage, ask AskFunc) (string, bool, error) {
	// Read-only tools skip the mutation gate, but two of them compose into
	// exfiltration (read a secret, POST it to a URL), and prompt injection
	// from fetched pages or repo files can drive exactly that. So they get
	// their own narrow checks below.
	if out, isErr, handled := e.gateReadOnly(name, raw, ask); handled {
		return out, isErr, nil
	}
	readOnly := name == "read_file" || name == "web_search" || name == "web_fetch"
	if !readOnly { // mutating tools go through the permission gate
		mode := e.Mode()
		fileEdit := name == "write_file" || name == "edit_file"
		needAsk := false
		switch mode {
		case ModePlan:
			if fileEdit {
				return "denied: plan mode is active — do not modify files; present a plan instead", true, nil
			}
			needAsk = true // bash still allowed for read-only inspection, but asks
		case ModeManual:
			needAsk = true
		case ModeAuto:
			needAsk = !fileEdit // edits auto-approved, bash asks
		case ModeBypass:
			needAsk = false
		}
		// Grants never outrank plan mode: an always-allow answered while in
		// auto must not let mutations slip through a later plan session.
		grantsApply := mode != ModePlan
		e.mu.Lock()
		blanket := grantsApply && e.allowed[name]
		e.mu.Unlock()
		if needAsk && !blanket && name == "bash" {
			// Compound commands are approved segment by segment; a denied
			// segment blocks the whole command.
			var a struct {
				Command string `json:"command"`
			}
			if json.Unmarshal(raw, &a) == nil && strings.TrimSpace(a.Command) != "" {
				for _, seg := range segments(a.Command) {
					if grantsApply && e.segmentAllowed(seg) {
						continue
					}
					d := ask(name, seg)
					if !d.Allow {
						return "the user denied permission for: " + seg, true, nil
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
						break
					}
				}
				needAsk = false
			}
		}
		if needAsk && !blanket {
			d := ask(name, e.summary(name, raw))
			if !d.Allow {
				return "the user denied permission for this call", true, nil
			}
			if d.Always {
				e.mu.Lock()
				e.allowed[name] = true
				e.mu.Unlock()
			}
		}
		e.mu.Lock()
		cp := e.checkpoint
		e.mu.Unlock()
		if cp != nil {
			cp(name + ": " + truncateLabel(e.summary(name, raw)))
		}
	}

	ctx, cancel := context.WithTimeout(ctx, execTimeout)
	defer cancel()
	switch name {
	case "bash":
		return e.runBash(ctx, raw)
	case "read_file":
		return e.readFile(raw)
	case "write_file":
		return e.writeFile(raw)
	case "edit_file":
		return e.editFile(raw)
	case "web_search":
		return e.webSearch(ctx, raw)
	case "web_fetch":
		return e.webFetch(ctx, raw)
	}
	return "", true, fmt.Errorf("unknown builtin tool %q", name)
}

// sensitiveNames are files whose contents are worth stealing regardless of
// where they live.
var sensitiveNames = []string{
	".env", "id_rsa", "id_ed25519", "id_ecdsa", "credentials",
	".netrc", ".pgpass", "kubeconfig", "shadow",
}

// sensitiveDirs are directories that never contain project source but do
// contain keys, tokens and browser data.
var sensitiveDirs = []string{
	".ssh", ".aws", ".gnupg", ".config/gh", ".config/gcloud", ".kube",
	".docker", ".mozilla", ".config/google-chrome", ".password-store",
	".local/share/keyrings",
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
	home, err := os.UserHomeDir()
	if err == nil {
		for _, d := range sensitiveDirs {
			full := filepath.Join(home, d)
			if clean == full || strings.HasPrefix(clean, full+string(filepath.Separator)) {
				return true
			}
		}
	}
	// Anything under the worktree is fair game; secrets elsewhere are not.
	cwd := e.Cwd()
	inTree := clean == cwd || strings.HasPrefix(clean, cwd+string(filepath.Separator))
	if !inTree {
		for _, d := range e.ExtraDirs() {
			abs, err := filepath.Abs(d)
			if err != nil {
				continue
			}
			if clean == abs || strings.HasPrefix(clean, abs+string(filepath.Separator)) {
				inTree = true
				break
			}
		}
	}
	return !inTree && looksSecret(strings.ToLower(clean))
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
	case "web_fetch":
		var a struct {
			URL string `json:"url"`
		}
		if json.Unmarshal(raw, &a) != nil || strings.TrimSpace(a.URL) == "" {
			return "", false, false
		}
		host := hostOf(a.URL)
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

func hostOf(raw string) string {
	if !strings.Contains(raw, "://") {
		raw = "https://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return strings.ToLower(u.Hostname())
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
	default:
		var a struct {
			Path string `json:"path"`
		}
		_ = json.Unmarshal(raw, &a)
		return a.Path
	}
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

func (e *Executor) runBash(ctx context.Context, raw json.RawMessage) (string, bool, error) {
	var a struct {
		Command string `json:"command"`
	}
	if err := json.Unmarshal(raw, &a); err != nil || strings.TrimSpace(a.Command) == "" {
		return "invalid arguments: need {\"command\": \"...\"}", true, nil
	}
	cmd := exec.CommandContext(ctx, "sh", "-c", a.Command)
	cmd.Dir = e.Cwd()
	cmd.Env = secretenv.Scrubbed()
	// Descendants holding the output pipe would make CombinedOutput block
	// past cancellation; kill the whole process group and cap the wait.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	cmd.WaitDelay = 10 * time.Second
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
	data, err := os.ReadFile(e.resolve(a.Path))
	if err != nil {
		return err.Error(), true, nil
	}
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

func (e *Executor) writeFile(raw json.RawMessage) (string, bool, error) {
	var a struct {
		Path    string `json:"path"`
		Content string `json:"content"`
	}
	if err := json.Unmarshal(raw, &a); err != nil || a.Path == "" {
		return "invalid arguments: need {\"path\", \"content\"}", true, nil
	}
	path := e.resolve(a.Path)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err.Error(), true, nil
	}
	if err := os.WriteFile(path, []byte(a.Content), 0o644); err != nil {
		return err.Error(), true, nil
	}
	return fmt.Sprintf("wrote %d bytes to %s", len(a.Content), path), false, nil
}

func (e *Executor) editFile(raw json.RawMessage) (string, bool, error) {
	var a struct {
		Path       string `json:"path"`
		OldString  string `json:"old_string"`
		NewString  string `json:"new_string"`
		ReplaceAll bool   `json:"replace_all"`
	}
	if err := json.Unmarshal(raw, &a); err != nil || a.Path == "" || a.OldString == "" {
		return "invalid arguments: need {\"path\", \"old_string\", \"new_string\"}", true, nil
	}
	path := e.resolve(a.Path)
	data, err := os.ReadFile(path)
	if err != nil {
		return err.Error(), true, nil
	}
	// Match against the same decoded text read_file shows, or UTF-16 files
	// are uneditable and Latin-1 files get UTF-8 spliced into them.
	content, isText := textenc.Decode(data)
	if !isText {
		return fmt.Sprintf("%s is a binary file; refusing to edit", a.Path), true, nil
	}
	reencoded := content != string(data)
	count := strings.Count(content, a.OldString)
	switch {
	case count == 0:
		return "old_string not found in file", true, nil
	case count > 1 && !a.ReplaceAll:
		return fmt.Sprintf("old_string appears %d times; make it unique or set replace_all", count), true, nil
	}
	if a.ReplaceAll {
		content = strings.ReplaceAll(content, a.OldString, a.NewString)
	} else {
		content = strings.Replace(content, a.OldString, a.NewString, 1)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		return err.Error(), true, nil
	}
	out := fmt.Sprintf("edited %s (%d replacement(s))", path, count)
	if reencoded {
		out += " — note: file was re-encoded to UTF-8"
	}
	return out, false, nil
}
