package tools

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

// Permission used to be a mode plus a list of always-allowed bash words, which
// could not express "edits under src/ never ask", "never touch .env" or
// anything at all about a specific MCP tool. Rules add that: each one names a
// tool and, optionally, what the call is about.
//
//	[permissions]
//	allow = ["bash(go test*)", "edit_file(internal/**)"]
//	ask   = ["bash(git push*)"]
//	deny  = ["read_file(**/.env)", "mcp:*__delete_*"]
//
// The part in parentheses matches the call's subject: the path for file tools,
// the command for bash (per segment, so a compound command is judged piece by
// piece), the URL for web_fetch, the query for web_search, and the JSON
// arguments for an MCP tool. Paths match like globs, where ** spans
// directories; everything else matches with * covering any run of characters.

// Rule outcomes, in the order they win: a deny cannot be talked round, an ask
// forces the prompt that auto or bypass would have skipped, and an allow skips
// a prompt that would otherwise have happened.
const (
	ruleNone = ""
	// Exported because the agent gates MCP tools itself.
	RuleAllow = "allow"
	RuleAsk   = "ask"
	RuleDeny  = "deny"
)

type rule struct {
	tool string // glob over the tool name
	arg  string // glob over the subject; empty matches every call
	act  string
}

// parseRule reads "tool(subject)" or plain "tool". A malformed rule is dropped
// rather than silently matching everything.
func parseRule(spec, act string) (rule, bool) {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return rule{}, false
	}
	if !strings.HasSuffix(spec, ")") {
		return rule{tool: spec, act: act}, true
	}
	open := strings.Index(spec, "(")
	if open <= 0 {
		return rule{}, false
	}
	tool := strings.TrimSpace(spec[:open])
	arg := strings.TrimSpace(spec[open+1 : len(spec)-1])
	if tool == "" {
		return rule{}, false
	}
	return rule{tool: tool, arg: arg, act: act}, true
}

// SetRules replaces the configured permission rules.
func (e *Executor) SetRules(allow, ask, deny []string) {
	var rules []rule
	// Order is precedence: the first match wins, so denials are checked first.
	for _, set := range []struct {
		specs []string
		act   string
	}{{deny, RuleDeny}, {ask, RuleAsk}, {allow, RuleAllow}} {
		for _, spec := range set.specs {
			if r, ok := parseRule(spec, set.act); ok {
				rules = append(rules, r)
			}
		}
	}
	e.mu.Lock()
	e.rules = rules
	e.mu.Unlock()
}

// wildcard matches with * for any run of characters and ? for one, without the
// path-segment rules a glob implies: a bash command is not a path.
func wildcard(pattern, s string) bool {
	if pattern == "" || pattern == "*" {
		return true
	}
	p, str := 0, 0
	star, mark := -1, 0
	for str < len(s) {
		switch {
		case p < len(pattern) && (pattern[p] == '?' || pattern[p] == s[str]):
			p, str = p+1, str+1
		case p < len(pattern) && pattern[p] == '*':
			star, mark = p, str
			p++
		case star >= 0:
			p, mark = star+1, mark+1
			str = mark
		default:
			return false
		}
	}
	for p < len(pattern) && pattern[p] == '*' {
		p++
	}
	return p == len(pattern)
}

// mutates reports whether a tool goes through the mutation gate. bash_output
// and bash_kill only observe or stop a command whose start was already
// approved, so they do not prompt again.
func mutates(tool string) bool {
	switch tool {
	case "read_file", "web_search", "web_fetch", "grep", "glob", "bash_output", "bash_kill":
		return false
	}
	return true
}

// pathRule reports whether a tool's subject is a filesystem path, which
// decides how its pattern is matched.
func pathRule(tool string) bool {
	switch tool {
	case "read_file", "write_file", "edit_file", "grep", "glob":
		return true
	}
	return false
}

// matchSubject compares one rule's pattern against a call's subject. Paths are
// matched both as written and resolved, so a rule can be relative to the
// working directory or absolute without the user having to guess which.
func (e *Executor) matchSubject(r rule, tool, subject string) bool {
	if r.arg == "" {
		return true
	}
	if !pathRule(tool) {
		return wildcard(r.arg, subject)
	}
	pattern := r.arg
	if strings.HasPrefix(pattern, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			pattern = filepath.Join(home, pattern[2:])
		}
	}
	subject = filepath.ToSlash(subject)
	if globMatch(filepath.ToSlash(pattern), subject) {
		return true
	}
	abs := filepath.ToSlash(e.resolve(subject))
	if globMatch(filepath.ToSlash(pattern), abs) {
		return true
	}
	if !filepath.IsAbs(pattern) {
		// A relative pattern is relative to the working directory.
		return globMatch(filepath.ToSlash(filepath.Join(e.cwdLocked(), pattern)), abs)
	}
	return false
}

func (e *Executor) cwdLocked() string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.cwd
}

// normalized rewrites a command into the form a rule is written in, so the
// spellings a shell treats as identical are matched identically. A rule says
// "rm *"; the shell also accepts "rm\t-rf", "\rm -rf", "'rm' -rf" and
// "/bin/rm -rf", and every one of those walked past a deny rule matched on raw
// text. Whitespace runs collapse to one space, the first word loses its
// quoting and its directory, and the result is matched alongside the original.
//
// Only denials and prompts are matched against it. Widening an allow the same
// way would let a stray executable named like a trusted one inherit its
// permission.
func normalized(command string) string {
	// A line continuation is whitespace to the shell.
	command = strings.ReplaceAll(command, "\\\n", " ")
	fields := strings.Fields(command) // splits on any run of whitespace
	// Quoting anywhere changes nothing about what runs, so every word is
	// unquoted, not only the first: deny bash(git push*) has to see git "push".
	for i := range fields {
		fields[i] = unquoteWord(fields[i])
	}
	// Drop the prefixes a shell steps over before it reaches the command:
	// environment assignments, a subshell or brace group, and negation.
	for len(fields) > 0 {
		w := fields[0]
		if w == "(" || w == "{" || w == "!" {
			fields = fields[1:]
			continue
		}
		if i := strings.IndexByte(w, '='); i > 0 && isName(w[:i]) {
			fields = fields[1:]
			continue
		}
		if len(w) > 1 && (w[0] == '(' || w[0] == '{' || w[0] == '!') {
			fields[0] = w[1:]
			continue
		}
		break
	}
	if len(fields) == 0 {
		return command
	}
	fields[0] = filepath.Base(strings.TrimRight(fields[0], ")};"))
	return strings.Join(fields, " ")
}

// isName reports whether s is a shell variable name, so that FOO=1 is
// recognised as an assignment and ./x=y is not.
func isName(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		ok := c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (i > 0 && c >= '0' && c <= '9')
		if !ok {
			return false
		}
	}
	return s != ""
}

// unquoteWord strips the quoting a shell removes before looking a command up:
// \rm, 'rm' and "rm" all run rm.
func unquoteWord(w string) string {
	var b strings.Builder
	for i := 0; i < len(w); i++ {
		switch w[i] {
		case '\\':
			if i+1 < len(w) {
				i++
				b.WriteByte(w[i])
			}
		case '\'', '"':
		default:
			b.WriteByte(w[i])
		}
	}
	return b.String()
}

// ruleFor returns the action configured for a call, or ruleNone.
func (e *Executor) ruleFor(tool, subject string) string {
	e.mu.Lock()
	rules := e.rules
	e.mu.Unlock()
	for _, r := range rules {
		if !wildcard(r.tool, tool) {
			continue
		}
		if e.matchSubject(r, tool, subject) {
			return r.act
		}
		if tool == "bash" && (r.act == RuleDeny || r.act == RuleAsk) {
			if norm := normalized(subject); norm != subject && e.matchSubject(r, tool, norm) {
				return r.act
			}
		}
	}
	return ruleNone
}

// RuleFor is ruleFor for callers outside this package: the agent gates MCP
// tools itself, and they take the same rules as the built-ins.
func (e *Executor) RuleFor(tool, subject string) string { return e.ruleFor(tool, subject) }

// subjectOf extracts what a rule matches against for a built-in call.
func subjectOf(tool string, raw json.RawMessage) string {
	var a struct {
		Path    string `json:"path"`
		Command string `json:"command"`
		URL     string `json:"url"`
		Query   string `json:"query"`
	}
	if json.Unmarshal(raw, &a) != nil {
		return ""
	}
	switch tool {
	case "bash":
		return strings.TrimSpace(a.Command)
	case "web_fetch":
		return strings.TrimSpace(a.URL)
	case "web_search":
		return strings.TrimSpace(a.Query)
	default:
		return strings.TrimSpace(a.Path)
	}
}

// deniedByRule is the message a deny rule produces. It names the rule's effect
// rather than the rule, so the model stops instead of rephrasing the call.
func deniedByRule(tool string) (string, bool, bool) {
	return "denied: a permission rule in the config forbids " + tool + " for this call", true, true
}
