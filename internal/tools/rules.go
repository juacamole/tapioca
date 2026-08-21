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
// matched as written, made absolute, and resolved, so a rule can be relative to
// the working directory or absolute without the user having to guess which.
//
// Both spellings of the working directory are used, because the two sides of
// the comparison did not agree about which one they were in. The subject is put
// through resolve(), which follows every link; the pattern was made absolute
// against the working directory as written. A working directory reached through
// a link is the ordinary case, not an exotic one — `cd ~/src` where ~/src is a
// link leaves that spelling in $PWD and os.Getwd hands it straight back, /tmp is
// a link on macOS, and /cd stores whatever was typed — and the two could then
// never match. deny = ["read_file(secrets/**)"] stopped denying the moment the
// model spelled the path absolutely, which it does, because the absolute
// working directory is in its system prompt. The same silence made an allow
// rule stop applying, so a user who wrote one to stop being asked was asked
// anyway.
//
// The subject's lexical absolute form is matched too, for the mirror image: a
// rule written against the link's spelling, and a call that arrives already
// resolved.
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
	cwd := e.cwdLocked()
	patterns := []string{pattern}
	if !filepath.IsAbs(pattern) {
		// A relative pattern is relative to the working directory — by both its
		// names, since the subject may have been resolved into either.
		patterns = append(patterns, filepath.Join(cwd, pattern))
		if real := realPath(filepath.Clean(cwd)); real != cwd && real != unresolvable {
			patterns = append(patterns, filepath.Join(real, pattern))
		}
	}
	lexical := subject
	if !filepath.IsAbs(lexical) {
		lexical = filepath.Join(cwd, lexical)
	}
	subjects := []string{subject, filepath.Clean(lexical), e.resolve(subject)}
	for _, p := range patterns {
		p = filepath.ToSlash(p)
		for _, s := range subjects {
			if globMatch(p, filepath.ToSlash(s)) {
				return true
			}
		}
	}
	return false
}

func (e *Executor) cwdLocked() string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.cwd
}

// normalizedForms rewrites a command into the forms a rule is written in, so
// the spellings a shell treats as identical are matched identically. A rule
// says "rm *"; the shell also accepts "rm\t-rf", "\rm -rf", "'rm' -rf" and
// "/bin/rm -rf", and every one of those walked past a deny rule matched on raw
// text. Whitespace runs collapse to one space, the first word loses its
// quoting and its directory, and the results are matched alongside the
// original.
//
// There is more than one form because a wrapper's options may or may not take
// a value, and no list of wrappers can say which: guessing that -u in
// `env -u FOO touch x` stood alone left "FOO" as the command word and the touch
// unmatched by any rule for touch. Every reading of the wrapper's arguments is
// emitted instead, and a rule that matches any of them wins.
//
// Only denials and prompts are matched against these. Widening an allow the
// same way would let a stray executable named like a trusted one inherit its
// permission.
//
// The second result says the reading is incomplete: the command word is one
// the shell builds at runtime, or the nesting ran past the budget below. The
// forms then say nothing about what runs, and a caller must not read their
// silence as the command being clear — see ruleFor.
func normalizedForms(command string) ([]string, bool) {
	s := &formScan{seen: map[string]bool{}, steps: maxFormSteps}
	s.scan(command, 0)
	return s.forms, s.opaque
}

// maxNesting bounds how far the readings below follow a substitution into
// another one, and maxFormSteps bounds the total work when the nesting
// branches instead of going straight down — `$(a $(b $(c …)))` with several
// substitutions per level is exponential in the depth alone. The model writes
// the command, so both are inputs it chooses. Nothing anyone writes goes near
// either.
//
// Hitting one is not "no forms": that is how a bounded reading becomes a way
// through it. `echo $($($(…$(touch pwned)…)))` nested seventeen deep emitted
// every outer form and none for the touch, so deny = ["bash(touch*)"] matched
// nothing and the touch ran — under bypass, where nothing prompts either.
// Running out of budget sets opaque, and the caller treats that as the rule
// matching rather than as an answer.
const (
	maxNesting   = 16
	maxFormSteps = 4096
)

// formScan accumulates the readings of one command. The budget and the
// verdict are shared across the whole recursion, because both are about the
// command as a whole rather than about any one substitution inside it.
type formScan struct {
	forms  []string
	seen   map[string]bool
	steps  int
	opaque bool
}

func (s *formScan) scan(command string, depth int) {
	if depth > maxNesting || s.steps <= 0 {
		s.opaque = true
		return
	}
	s.steps--
	// A line continuation is deleted by the shell, not turned into whitespace:
	// `to\<newline>uch x` is a touch. Replacing it with a space read that as
	// `to uch x`, which matched no rule for touch and ran under bypass.
	command = strings.ReplaceAll(command, "\\\n", "")
	// A lone & is a command separator that segments() leaves in place, so the
	// program after it was never the first word of anything and no rule for it
	// could match: `true & touch pwned` reached the rules as a `true`, and
	// deny = ["bash(touch*)"] ran the touch — under bypass, where nothing
	// prompts either. Each side is read on its own here.
	if parts := backgroundParts(command); len(parts) > 1 {
		for _, p := range parts {
			s.scan(p, depth+1)
		}
		return
	}
	raw := strings.Fields(command) // splits on any run of whitespace
	// Quoting anywhere changes nothing about what runs, so every word is
	// unquoted, not only the first: deny bash(git push*) has to see git "push".
	fields := make([]string, len(raw))
	for i := range raw {
		fields[i] = unquoteWord(raw[i])
	}
	emit := func(f []string) {
		if len(f) == 0 {
			return
		}
		g := append([]string(nil), f...)
		g[0] = filepath.Base(strings.TrimRight(g[0], ")};"))
		if str := strings.Join(g, " "); !s.seen[str] {
			s.seen[str] = true
			s.forms = append(s.forms, str)
		}
	}
	// Drop everything a shell steps over before it reaches the command:
	// environment assignments, a subshell or brace group, negation, the
	// keywords that introduce a command, redirections, and wrappers that run
	// their arguments.
	for len(fields) > 0 {
		w := fields[0]
		if w == "(" || w == "{" || w == "!" || reservedWords[w] || isRedirection(w) {
			fields, raw = fields[1:], raw[1:]
			continue
		}
		if isAssignment(w) {
			fields, raw = fields[1:], raw[1:]
			continue
		}
		if len(w) > 1 && (w[0] == '(' || w[0] == '{' || w[0] == '!') {
			fields[0] = w[1:]
			if len(raw[0]) > 1 && raw[0][0] == w[0] {
				raw[0] = raw[0][1:]
			}
			continue
		}
		if len(fields) > 1 && transparentWrappers[filepath.Base(w)] {
			fields, raw = fields[1:], raw[1:]
			prev := ""
			for len(fields) > 1 {
				emit(fields) // this word may already be the command
				// Stop at a word that is neither an option, an operand of the
				// wrapper, nor the possible value of the option before it.
				if !isWrapperArg(fields[0]) && !strings.HasPrefix(prev, "-") {
					break
				}
				prev = fields[0]
				fields, raw = fields[1:], raw[1:]
			}
			continue
		}
		break
	}
	// The word left here is the one that becomes the program, and a word the
	// shell rewrites first is not a program name any reading can spell:
	// `C=touch; $C x`, `t${x}ouch x`, `t?uch x` and `~/bin/x` all run something
	// none of the forms below name. Emitting "$C x" and letting a rule for
	// touch fail to match it reported "no rule applies" for a command nobody
	// could read; saying the reading is opaque is the only honest answer. The
	// guesses inside the wrapper loop above are not judged this way — the word
	// they emit is as likely to be the wrapper's own argument, and
	// `env FOO=$BAR make test` is not an unreadable command.
	if len(raw) > 0 && opaqueCommandWord(raw[0]) {
		s.opaque = true
	}
	emit(fields)
	// A command substitution runs a command list of its own, and the program
	// inside it is never the first word of anything: `echo $(touch pwned)`,
	// `echo \`touch pwned\`` and `cat <(touch pwned)` all reached the rules as
	// an echo or a cat, so deny = ["bash(touch*)"] matched nothing and the
	// touch ran. The body is split and read exactly like a top-level command,
	// which also covers one nested inside another.
	for _, body := range substitutionBodies(command) {
		for _, seg := range segments(body) {
			s.scan(seg, depth+1)
		}
	}
}

// substitutionBodies returns the command lists inside a segment's
// substitutions: $(…), `…` and the process substitutions <(…) and >(…).
// Single quotes suppress all of them; double quotes suppress none.
func substitutionBodies(seg string) []string {
	var bodies []string
	inSingle, inDouble := false, false
	for i := 0; i < len(seg); i++ {
		c := seg[i]
		switch {
		case c == '\\' && !inSingle:
			i++
		case c == '\'' && !inDouble:
			inSingle = !inSingle
		case c == '"' && !inSingle:
			inDouble = !inDouble
		case inSingle:
		case c == '`':
			if j := strings.IndexByte(seg[i+1:], '`'); j >= 0 {
				bodies = append(bodies, seg[i+1:i+1+j])
				i += j + 1
			}
		case c == '$' && i+1 < len(seg) && seg[i+1] == '(':
			body, end := parenBody(seg, i+1)
			if end > 0 {
				bodies = append(bodies, body)
				i = end
			}
		case (c == '<' || c == '>') && i+1 < len(seg) && seg[i+1] == '(' && !inDouble:
			body, end := parenBody(seg, i+1)
			if end > 0 {
				bodies = append(bodies, body)
				i = end
			}
		}
	}
	return bodies
}

// parenBody returns what is between the '(' at open and its matching ')', and
// the index of that ')'. Nesting and quoting are tracked, so the inner ')' of
// `$(echo $(date))` and the one in `$(echo ")")` do not end it. An unbalanced
// run returns an end of 0, and the caller reads nothing from it.
func parenBody(seg string, open int) (string, int) {
	depth, inSingle, inDouble := 0, false, false
	for i := open; i < len(seg); i++ {
		switch c := seg[i]; {
		case c == '\\' && !inSingle:
			i++
		case c == '\'' && !inDouble:
			inSingle = !inSingle
		case c == '"' && !inSingle:
			inDouble = !inDouble
		case inSingle || inDouble:
		case c == '(':
			depth++
		case c == ')':
			if depth--; depth == 0 {
				return seg[open+1 : i], i
			}
		}
	}
	return "", 0
}

// backgroundParts splits on the & that backgrounds a command, respecting
// quotes. An & that belongs to a redirection is not one: 2>&1 and &>log are
// part of the command they follow, and splitting there would invent a segment
// out of "1" and read the rest as something nobody wrote.
func backgroundParts(seg string) []string {
	var parts []string
	var cur strings.Builder
	inSingle, inDouble := false, false
	for i := 0; i < len(seg); i++ {
		c := seg[i]
		switch {
		case c == '\\' && !inSingle:
			cur.WriteByte(c)
			if i+1 < len(seg) {
				i++
				cur.WriteByte(seg[i])
			}
		case c == '\'' && !inDouble:
			inSingle = !inSingle
			cur.WriteByte(c)
		case c == '"' && !inSingle:
			inDouble = !inDouble
			cur.WriteByte(c)
		case c == '&' && !inSingle && !inDouble && !redirectionAmp(seg, i):
			parts = append(parts, cur.String())
			cur.Reset()
		default:
			cur.WriteByte(c)
		}
	}
	return append(parts, cur.String())
}

// redirectionAmp reports whether the & at i is part of a redirection rather
// than a separator: the & in 2>&1 and <&0 follows the operator, the one in
// &>log precedes it.
func redirectionAmp(seg string, i int) bool {
	if i > 0 && (seg[i-1] == '>' || seg[i-1] == '<') {
		return true
	}
	return i+1 < len(seg) && seg[i+1] == '>'
}

// reservedWords are shell keywords that stand in front of a command instead of
// being one. segments() hands over `then touch x` and `until touch x` with the
// keyword still attached, and a deny rule for touch matched neither.
// Only keywords a command word can directly follow are listed: `for` and `case`
// are followed by a name.
var reservedWords = map[string]bool{
	"if": true, "then": true, "elif": true, "else": true,
	"while": true, "until": true, "do": true, "coproc": true,
}

// isAssignment reports whether a word is a variable assignment standing in
// front of a command, so that FOO=1 is stepped over and ./x=y is not. Matching
// only sh's name=value was not enough: bash also takes name+=value and
// name[index]=value in that position, and `FOO+=1 rm -rf ~` therefore reached
// the shell as an rm that no deny rule for rm had matched.
func isAssignment(w string) bool {
	i := 0
	for i < len(w) {
		c := w[i]
		alpha := c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
		if !alpha && !(i > 0 && c >= '0' && c <= '9') {
			break
		}
		i++
	}
	if i == 0 {
		return false // no name: not an assignment
	}
	if i < len(w) && w[i] == '[' { // an array element: a[0]=1, a[$k]=1
		j := strings.IndexByte(w[i:], ']')
		if j < 0 {
			return false
		}
		i += j + 1
	}
	if i < len(w) && w[i] == '+' { // append: FOO+=1
		i++
	}
	return i < len(w) && w[i] == '='
}

// unquoteWord strips the quoting a shell removes before looking a command up:
// \rm, 'rm', "rm", $'rm' and $"rm" all run rm.
func unquoteWord(w string) string {
	var b strings.Builder
	for i := 0; i < len(w); i++ {
		switch w[i] {
		case '\\':
			if i+1 < len(w) {
				i++
				b.WriteByte(w[i])
			}
		case '$':
			// $'...' is ANSI-C quoting and $"..." is locale translation, which
			// yields the string itself when nothing is translated. In both the
			// $ is not part of the word, and treating $"push" as a word
			// containing a '$' is what let it past a deny rule for git push.
			if i+1 < len(w) && w[i+1] == '\'' {
				// Inside $'…' a backslash is an escape with a value, not a way
				// of writing the next byte: $'\164ouch' and $'\x74ouch' are
				// both `touch`, and stripping the backslash left "164ouch",
				// which no rule for touch matched and which ran under bypass.
				text, end := ansiCString(w, i+1)
				b.WriteString(text)
				i = end
				continue
			}
			if i+1 >= len(w) || w[i+1] != '"' {
				b.WriteByte(w[i])
			}
		case '\'', '"':
		default:
			b.WriteByte(w[i])
		}
	}
	return b.String()
}

// ansiCString decodes the $'…' beginning at the quote at open, and returns
// what it yields and the index of the closing quote. An unterminated run ends
// at the end of the word, which is what a shell reports as an error and what
// leaves the fewest bytes unread here.
func ansiCString(w string, open int) (string, int) {
	var b strings.Builder
	for i := open + 1; i < len(w); i++ {
		if w[i] == '\'' {
			return b.String(), i
		}
		if w[i] != '\\' || i+1 >= len(w) {
			b.WriteByte(w[i])
			continue
		}
		i++
		switch c := w[i]; c {
		case 'a':
			b.WriteByte(7)
		case 'b':
			b.WriteByte(8)
		case 'e', 'E':
			b.WriteByte(27)
		case 'f':
			b.WriteByte(12)
		case 'n':
			b.WriteByte('\n')
		case 'r':
			b.WriteByte('\r')
		case 't':
			b.WriteByte('\t')
		case 'v':
			b.WriteByte(11)
		case 'x', 'u', 'U':
			// Hex, in the widths bash accepts. A \u is written out as UTF-8,
			// so a rule matching plain text sees the same bytes the shell
			// hands the kernel.
			width := map[byte]int{'x': 2, 'u': 4, 'U': 8}[c]
			n, digits := 0, 0
			for digits < width && i+1 < len(w) && isHexDigit(w[i+1]) {
				n = n*16 + hexValue(w[i+1])
				i, digits = i+1, digits+1
			}
			if digits == 0 {
				b.WriteByte(c) // not an escape after all: $'\xz' is "xz"
			} else if c == 'x' {
				b.WriteByte(byte(n))
			} else {
				b.WriteRune(rune(n))
			}
		case '0', '1', '2', '3', '4', '5', '6', '7':
			n, digits := 0, 0
			for digits < 3 && i < len(w) && w[i] >= '0' && w[i] <= '7' {
				n = n*8 + int(w[i]-'0')
				i, digits = i+1, digits+1
			}
			i-- // the loop above stepped past the last digit
			b.WriteByte(byte(n))
		case 'c':
			// \cX is the control character for X; \c\ is a spelling of it too.
			if i+1 < len(w) {
				i++
				b.WriteByte(w[i] & 0x1f)
			}
		default:
			b.WriteByte(c) // \\ , \' , \" and anything else stand for themselves
		}
	}
	return b.String(), len(w) - 1
}

func isHexDigit(c byte) bool {
	return (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')
}

func hexValue(c byte) int {
	switch {
	case c >= '0' && c <= '9':
		return int(c - '0')
	case c >= 'a' && c <= 'f':
		return int(c-'a') + 10
	default:
		return int(c-'A') + 10
	}
}

// opaqueCommandWord reports whether the shell rewrites a word before it
// becomes a command name, so that what runs cannot be read off the text.
//
// The one exception is the test builtin: `[ -f x ] && …` begins a segment with
// a bracket, which expandsWord reads as the start of a glob. A lone bracket
// has no closing one to pair with, so the shell leaves it alone and runs
// `[` — and treating every `if [ -f x ]` as unreadable would put a deny rule
// for one command in front of a construct that has nothing to do with it.
func opaqueCommandWord(w string) bool {
	if w == "[" || w == "[[" {
		return false
	}
	// A leading ~ is stripped rather than treated as unreadable: what it
	// expands to is a directory path, so the program's name — the last
	// component, which is what emit() puts in the form — is the same either
	// way. Calling `~/go/bin/golangci-lint run` unreadable meant every deny
	// and ask rule matched it, so a rule about `rm -rf` refused a linter.
	return expandsWord(tildeHead(w))
}

// transparentWrappers run their arguments as a command, so the program that
// ends up running is not the first word. They are dropped before matching, so
// a deny rule for a program also covers `env <program>`.
// A shell is the plainest case of all: `sh -c 'touch pwned'` is a touch, and
// reading it as an `sh` left every deny rule for touch unmatched.
var transparentWrappers = map[string]bool{
	"env": true, "command": true, "builtin": true, "exec": true, "eval": true,
	"nohup": true, "setsid": true, "nice": true, "ionice": true,
	"stdbuf": true, "time": true, "timeout": true, "xargs": true,
	"sudo": true, "doas": true,
	"sh": true, "bash": true, "zsh": true, "dash": true, "ksh": true,
	"fish": true, "busybox": true,
}

// isRedirection reports whether a word is a redirection rather than a command.
// POSIX allows them before the command word: `>/dev/null rm x` runs rm.
func isRedirection(w string) bool {
	i := 0
	for i < len(w) && w[i] >= '0' && w[i] <= '9' {
		i++
	}
	return i < len(w) && (w[i] == '>' || w[i] == '<')
}

// isWrapperArg reports whether a word is an option or operand belonging to the
// wrapper in front of it rather than the command being wrapped: the 5 in
// `timeout 5 rm`, the -n 9 in `nice -n 9 rm`, the FOO=1 in `env FOO=1 rm`.
func isWrapperArg(w string) bool {
	if strings.HasPrefix(w, "-") || strings.Contains(w, "=") {
		return true
	}
	for i := 0; i < len(w); i++ {
		if w[i] < '0' || w[i] > '9' {
			return false
		}
	}
	return w != ""
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
			forms, opaque := normalizedForms(subject)
			for _, norm := range forms {
				if norm != subject && e.matchSubject(r, tool, norm) {
					return r.act
				}
			}
			// The forms did not match, and they could not be completed: the
			// command word is built at runtime, or the nesting ran past the
			// budget. "No form matched" is then not a finding about this
			// command, and reading it as one is how `$($($(…$(touch pwned)…)))`
			// and `C=touch; $C pwned` walked past a rule for touch. The rule the
			// user wrote decides what happens to a command nobody can read.
			if opaque {
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

// deniedByUser is what an unanswered or refused prompt hands back.
const deniedByUser = "the user denied permission for this call"
