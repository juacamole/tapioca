package tools

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// This file only asserts ORDINARY behaviour: what a user doing everyday work
// expects to happen without a prompt and without a denial. Nothing here is a
// security test; a failure means the hardening reached past what it was aimed
// at.

// logAsk records every prompt and answers yes, so a test can tell "ran without
// asking" from "ran because the fake user said yes".
func logAsk(log *[]string) AskFunc {
	return func(tool, summary string) Decision {
		*log = append(*log, tool+": "+summary)
		return Decision{Allow: true}
	}
}

func bashArgs(t *testing.T, cmd string) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(map[string]string{"command": cmd})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// A [p] grant on the command word is the shortcut the UI offers for exactly
// these commands. Every one of them must then run with no further prompt.
func TestOrdinaryEverydayCommandsRunUnderAPrefixGrant(t *testing.T) {
	cmds := []string{
		"git status",
		"git diff",
		"git log --oneline -5",
		`git commit -m "fix: thing"`,
		"go build ./...",
		"go test ./...",
		"npm ci",
		"npm run build",
		"make",
		"ls -la",
		"cat README.md",
	}
	for _, cmd := range cmds {
		t.Run(cmd, func(t *testing.T) {
			e := NewExecutor(t.TempDir(), "manual")
			e.SetBashPrefixes([]string{"git", "go", "npm", "make", "ls", "cat"})
			var log []string
			denial, ok := e.Approve("bash", bashArgs(t, cmd), logAsk(&log))
			if !ok {
				t.Fatalf("%q was refused: %s", cmd, denial)
			}
			if len(log) > 0 {
				t.Errorf("%q prompted despite a [p] grant on its command word: %v", cmd, log)
			}
		})
	}
}

// The same list, at the level the grant is actually matched, so a failure says
// which of PrefixGrantable/segmentAllowed disagreed.
func TestOrdinaryEverydayCommandsAreGrantable(t *testing.T) {
	e := NewExecutor(t.TempDir(), "manual")
	e.SetBashPrefixes([]string{"git", "go", "npm", "make", "ls", "cat"})
	for _, cmd := range []string{
		"git status", "git diff", "git log --oneline -5", `git commit -m "fix: thing"`,
		"go build ./...", "go test ./...", "npm ci", "npm run build",
		"make", "ls -la", "cat README.md",
	} {
		if !PrefixGrantable(cmd) {
			t.Errorf("[p] is not even offered for %q", cmd)
		}
		if !e.segmentAllowed(cmd) {
			t.Errorf("an existing grant does not cover %q", cmd)
		}
	}
}

// A command the user typed across several lines with backslash continuations is
// one command. The shell deletes the continuation; the gate must not turn it
// into something else, and must not ask twice.
func TestOrdinaryBackslashContinuationIsOneOrdinaryCommand(t *testing.T) {
	cmd := "git commit \\\n  -m \"fix: thing\" \\\n  --no-verify"
	e := NewExecutor(t.TempDir(), "manual")
	e.SetBashPrefixes([]string{"git"})
	var log []string
	denial, ok := e.Approve("bash", bashArgs(t, cmd), logAsk(&log))
	if !ok {
		t.Fatalf("a continued git commit was refused: %s", denial)
	}
	if len(log) > 0 {
		t.Errorf("a continued git commit prompted %d time(s) despite a grant on git: %v", len(log), log)
	}
}

// A `go build` split over continuation lines, likewise.
func TestOrdinaryContinuedGoBuildIsOneOrdinaryCommand(t *testing.T) {
	cmd := "go build \\\n  -o bin/app \\\n  ./cmd/app"
	e := NewExecutor(t.TempDir(), "manual")
	e.SetBashPrefixes([]string{"go"})
	var log []string
	if _, ok := e.Approve("bash", bashArgs(t, cmd), logAsk(&log)); !ok {
		t.Fatal("a continued go build was refused")
	}
	if len(log) > 0 {
		t.Errorf("a continued go build prompted: %v", log)
	}
}

// A heredoc costs one prompt per body line, and prose in the body is matched
// against the rules as if it were a command. That is a real cost — "bash: EOF"
// is not a prompt anyone can answer sensibly — and it is recorded here rather
// than fixed, because the fix is worse than the cost.
//
// segments() splits on newlines, which is why the body arrives as separate
// commands. Attaching the body to the command that introduced it would be
// right for `cat` and wrong for `sh`: a heredoc fed to a shell *is* a list of
// commands, and today `sh <<EOF … touch pwned … EOF` reaches a deny rule for
// touch because line three is its own segment. Telling those two apart means
// deciding which commands read their heredoc as code — a list, of the kind
// every earlier round of this audit was beaten by. Both symptoms here are the
// fail-closed direction: extra prompts and a refusal, never a command nobody
// saw. So the invariant worth pinning is that nothing runs unseen.
func TestOrdinaryHeredocShowsEveryLineToTheUser(t *testing.T) {
	cmd := "cat > notes.txt <<'EOF'\nhello there\nsecond line\nEOF"
	e := NewExecutor(t.TempDir(), "manual")
	e.SetBashPrefixes([]string{"cat"})
	var log []string
	denial, ok := e.Approve("bash", bashArgs(t, cmd), logAsk(&log))
	if !ok {
		t.Fatalf("an ordinary heredoc was refused: %s", denial)
	}
	for _, body := range []string{"hello there", "second line", "EOF"} {
		found := false
		for _, p := range log {
			found = found || p == "bash: "+body
		}
		if !found {
			t.Errorf("the heredoc body line %q was neither shown nor covered: %v", body, log)
		}
	}
}

// The same cost from the other side: a deny rule aimed at rm matches a line of
// prose inside a heredoc. A refusal is the safe direction, and the model is
// told which segment did it, so it can write the file with write_file instead.
func TestOrdinaryHeredocProseIsRefusedRatherThanRun(t *testing.T) {
	e := NewExecutor(t.TempDir(), "bypass")
	e.SetRules(nil, nil, []string{"bash(rm*)", "bash(curl*)"})
	cmd := "cat > notes.txt <<'EOF'\nrm the old build dir by hand\nEOF"
	var log []string
	denial, ok := e.Approve("bash", bashArgs(t, cmd), logAsk(&log))
	if ok {
		t.Fatal("a deny rule stopped matching inside a heredoc; if that was deliberate, `sh <<EOF` needs a test of its own")
	}
	if !strings.Contains(denial, "rm the old build dir") {
		t.Errorf("the refusal does not name the segment that caused it: %s", denial)
	}
}

// A symlinked working directory is ordinary: /tmp on macOS, ~/src -> /data/src,
// a corporate home. Nothing in the project may then read as outside it.
func TestOrdinarySymlinkedCwdIsNotOutsideItself(t *testing.T) {
	real := t.TempDir()
	link := filepath.Join(t.TempDir(), "work")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := os.WriteFile(filepath.Join(real, "main.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	e := NewExecutor(link, "auto")

	// edit_file / write_file: auto mode approves edits to the project.
	var log []string
	raw, _ := json.Marshal(map[string]string{
		"path": "main.go", "content": "package main\n\nfunc main() {}\n",
	})
	if denial, ok := e.Approve("write_file", raw, logAsk(&log)); !ok {
		t.Fatalf("write in a symlinked cwd refused: %s", denial)
	}
	for _, p := range log {
		t.Errorf("write_file in a symlinked cwd prompted: %s", p)
	}

	// grep / glob / read_file go through the read-only gate.
	for _, tool := range []string{"grep", "glob"} {
		var rlog []string
		args, _ := json.Marshal(map[string]string{"pattern": "package", "path": link})
		out, denied, _ := e.gateReadOnly(tool, args, logAsk(&rlog))
		if denied {
			t.Errorf("%s in a symlinked cwd was denied: %s", tool, out)
		}
		for _, p := range rlog {
			t.Errorf("%s in a symlinked cwd prompted: %s", tool, p)
		}
	}
	var rlog []string
	args, _ := json.Marshal(map[string]string{"path": filepath.Join(link, "main.go")})
	if out, denied, _ := e.gateReadOnly("read_file", args, logAsk(&rlog)); denied {
		t.Errorf("read_file in a symlinked cwd was denied: %s", out)
	}
	for _, p := range rlog {
		t.Errorf("read_file in a symlinked cwd prompted: %s", p)
	}
	if !e.inWorkArea(e.resolve("main.go")) {
		t.Error("a file in a symlinked cwd is judged outside the working directory")
	}
}

// A grep rooted at a relative path inside a symlinked cwd is the ordinary case
// the model produces.
func TestOrdinarySymlinkedCwdRelativeGrep(t *testing.T) {
	real := t.TempDir()
	link := filepath.Join(t.TempDir(), "work")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(real, "internal"), 0o755); err != nil {
		t.Fatal(err)
	}
	e := NewExecutor(link, "auto")
	var log []string
	args, _ := json.Marshal(map[string]string{"pattern": "x", "path": "internal"})
	if out, denied, _ := e.gateReadOnly("grep", args, logAsk(&log)); denied {
		t.Errorf("relative grep in a symlinked cwd denied: %s", out)
	}
	for _, p := range log {
		t.Errorf("relative grep in a symlinked cwd prompted: %s", p)
	}
}

// The documented allow rule has to do what the doc comment in rules.go says.
func TestOrdinaryAllowRuleCoversGoTest(t *testing.T) {
	e := NewExecutor(t.TempDir(), "manual")
	e.SetRules([]string{"bash(go test*)"}, nil, nil)
	for _, cmd := range []string{"go test ./...", "go test -run TestX ./internal/tools"} {
		var log []string
		denial, ok := e.Approve("bash", bashArgs(t, cmd), logAsk(&log))
		if !ok {
			t.Fatalf("%q refused despite allow = [\"bash(go test*)\"]: %s", cmd, denial)
		}
		if len(log) > 0 {
			t.Errorf("%q prompted despite allow = [\"bash(go test*)\"]: %v", cmd, log)
		}
	}
}

// Shell constructs a person writes every day must not be read as "unreadable",
// because an unreadable command takes the action of whatever deny or ask rule
// happens to be configured — for an entirely unrelated command.
func TestOrdinaryShellConstructsAreNotOpaque(t *testing.T) {
	ordinary := []string{
		`if [ -f x ]; then echo hi; fi`,
		`for f in *.go; do echo $f; done`,
		`env FOO=1 make test`,
		`while read l; do echo $l; done < list`,
		`case $1 in a) echo a;; esac`,
		`ls -la`,
		`go test ./...`,
	}
	for _, cmd := range ordinary {
		for _, seg := range segments(cmd) {
			if _, opaque := normalizedForms(seg); opaque {
				t.Errorf("segment %q of %q reads as unreadable", seg, cmd)
			}
		}
	}
}

// The same, at the level that decides what happens: an unrelated deny rule must
// not reach these.
func TestOrdinaryShellConstructsDoNotTripAnUnrelatedDenyRule(t *testing.T) {
	e := NewExecutor(t.TempDir(), "bypass")
	e.SetRules(nil, nil, []string{"bash(rm -rf*)"})
	for _, cmd := range []string{
		`if [ -f x ]; then echo hi; fi`,
		`for f in *.go; do echo $f; done`,
		`env FOO=1 make test`,
		`while read l; do echo $l; done < list`,
		`case $1 in a) echo a;; esac`,
		`for i in 1 2 3; do go test ./pkg$i; done`,
		`if command -v go >/dev/null; then go build ./...; fi`,
	} {
		var log []string
		denial, ok := e.Approve("bash", bashArgs(t, cmd), logAsk(&log))
		if !ok {
			t.Errorf("%q was denied by a rule for `rm -rf`: %s", cmd, denial)
		}
	}
}

// And with an ask rule, which is the same mechanism pointed at prompting.
func TestOrdinaryShellConstructsDoNotTripAnUnrelatedAskRule(t *testing.T) {
	e := NewExecutor(t.TempDir(), "bypass")
	e.SetRules(nil, []string{"bash(git push*)"}, nil)
	for _, cmd := range []string{
		`if [ -f x ]; then echo hi; fi`,
		`for f in *.go; do echo $f; done`,
		`env FOO=1 make test`,
		`case $1 in a) echo a;; esac`,
	} {
		var log []string
		if _, ok := e.Approve("bash", bashArgs(t, cmd), logAsk(&log)); !ok {
			t.Errorf("%q refused", cmd)
		}
		if len(log) > 0 {
			t.Errorf("%q prompted because of an ask rule for `git push`: %v", cmd, log)
		}
	}
}

// A tool installed in the user's own bin directory is named with a tilde every
// day: `~/go/bin/golangci-lint run`, `~/.local/bin/ruff check .`. The word is
// one the shell rewrites, so the readings are marked unreadable — and an
// unreadable command takes the action of whatever deny rule happens to exist,
// for a completely unrelated command.
func TestOrdinaryTildeToolPathIsNotDeniedByAnUnrelatedRule(t *testing.T) {
	e := NewExecutor(t.TempDir(), "bypass")
	e.SetRules(nil, nil, []string{"bash(rm -rf*)"})
	for _, cmd := range []string{
		"~/go/bin/golangci-lint run",
		"~/.local/bin/ruff check .",
		"~/bin/mytool --version",
	} {
		var log []string
		if denial, ok := e.Approve("bash", bashArgs(t, cmd), logAsk(&log)); !ok {
			t.Errorf("%q was denied by a rule for `rm -rf`: %s", cmd, denial)
		}
	}
}

// The same word under an ask rule for something unrelated: an ordinary tool run
// must not start prompting because `git push` has a rule.
func TestOrdinaryTildeToolPathDoesNotPromptForAnUnrelatedAskRule(t *testing.T) {
	e := NewExecutor(t.TempDir(), "bypass")
	e.SetRules(nil, []string{"bash(git push*)"}, nil)
	var log []string
	if _, ok := e.Approve("bash", bashArgs(t, "~/go/bin/golangci-lint run"), logAsk(&log)); !ok {
		t.Fatal("refused")
	}
	if len(log) > 0 {
		t.Errorf("an ordinary ~/bin tool prompted because of an ask rule for `git push`: %v", log)
	}
}

// What a [p] grant on git, go and make does and does not cover. The ones left
// out cost a prompt every time, which is a real cost on `git submodule update
// --init --recursive` — most checkouts need it — and it is the deliberate
// answer: submodule.<name>.update = "!cmd" in a .git/config the archive wrote
// runs a program, `git config` writes the keys that make every later git run
// one, and `make -f` picks the file whose recipes run. A grant is a promise
// that nobody reads the next command; these are the ones where that promise
// cannot be kept. The reads that are ordinary and safe stay covered.
func TestOrdinaryGrantCoversTheCommandsItCan(t *testing.T) {
	e := NewExecutor(t.TempDir(), "manual")
	e.SetBashPrefixes([]string{"git", "go", "make"})
	for _, cmd := range []string{
		"git status", "git diff --stat", "git log --oneline -5", "git fetch --all",
		"go env GOPATH", "go build ./...", "make", "make test",
	} {
		if !e.segmentAllowed(cmd) {
			t.Errorf("a [p] grant on the command word does not cover %q", cmd)
		}
	}
	for _, cmd := range []string{
		"git submodule update --init --recursive",
		"git config user.name",
		"make -f Makefile.dev test",
	} {
		if e.segmentAllowed(cmd) {
			t.Errorf("a [p] grant covered %q, which can run a program the grant never named", cmd)
		}
	}
}

// Ordinary variable use inside a command a grant covers. `git commit -m "$MSG"`
// and `make -j$(nproc)` are everyday; they may prompt (a substitution is a fair
// reason), but they must never be denied outright.
func TestOrdinaryVariableUseIsNotDenied(t *testing.T) {
	e := NewExecutor(t.TempDir(), "bypass")
	e.SetRules(nil, nil, []string{"bash(rm -rf*)"})
	for _, cmd := range []string{
		`git commit -m "$MSG"`,
		`make -j$(nproc)`,
		`echo "building $(git rev-parse --short HEAD)"`,
	} {
		var log []string
		if denial, ok := e.Approve("bash", bashArgs(t, cmd), logAsk(&log)); !ok {
			t.Errorf("%q was denied by a rule for `rm -rf`: %s", cmd, denial)
		}
	}
}
