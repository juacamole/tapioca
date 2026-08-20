package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// runBash runs one command through the whole gate under bypass — the mode that
// prompts for nothing, so a deny rule is the only thing left — and reports
// whether the model was refused.
func runBash(t *testing.T, dir string, deny []string, command string) (refused bool) {
	t.Helper()
	e := NewExecutor(dir, ModeBypass)
	e.SetRules(nil, nil, deny)
	raw, err := json.Marshal(map[string]string{"command": command})
	if err != nil {
		t.Fatal(err)
	}
	out, _, err := e.Call(context.Background(), "bash", raw, func(string, string) Decision {
		return Decision{Allow: true}
	})
	if err != nil {
		return true
	}
	return strings.Contains(out, "denied")
}

// deniedEndToEnd asserts that a spelling of `touch <marker>` really does create
// the marker when nothing forbids it, and really does not when a deny rule for
// touch is in force. The first half is the control: without it the second half
// would pass for a command that never worked in this shell at all.
func deniedEndToEnd(t *testing.T, command string) {
	t.Helper()

	loose := t.TempDir()
	marker := filepath.Join(loose, "PWNED")
	if runBash(t, loose, nil, strings.ReplaceAll(command, "MARKER", marker)) {
		t.Fatalf("control: %q was refused with no rules configured", command)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("control: %q did not create the marker, so this test cannot show anything: %v", command, err)
	}

	strict := t.TempDir()
	marker = filepath.Join(strict, "PWNED")
	refused := runBash(t, strict, []string{"bash(touch*)"}, strings.ReplaceAll(command, "MARKER", marker))
	if _, err := os.Stat(marker); err == nil {
		t.Errorf("%q ran under deny = [\"bash(touch*)\"]", command)
	}
	if !refused {
		t.Errorf("%q was not refused by deny = [\"bash(touch*)\"]", command)
	}
}

// The readings of a command are bounded, because the model writes the command
// and can nest a substitution as deep as it likes. Running out of budget used
// to mean "no forms", which a rule read as "nothing matched": seventeen levels
// of $( ) hid the touch from every form and it ran under bypass, where nothing
// prompts either.
func TestNestingBudgetIsNotAWayThrough(t *testing.T) {
	for _, depth := range []int{1, maxNesting, maxNesting + 1, maxNesting + 9} {
		cmd := "touch MARKER"
		for i := 0; i < depth; i++ {
			cmd = "$(" + cmd + ")"
		}
		deniedEndToEnd(t, "echo "+cmd)
	}
}

// $'…' is ANSI-C quoting, where a backslash introduces an escape with a value
// rather than a way of writing the next byte. Stripping the backslash read
// $'\164ouch' as "164ouch", which matched no rule for touch while the shell
// ran one.
func TestAnsiCEscapesAreReadAsTheShellReadsThem(t *testing.T) {
	for _, form := range []string{
		`$'touch' MARKER`,
		`$'\164ouch' MARKER`, // octal
		`$'\x74ouch' MARKER`, // hex
		`to$'\165'ch MARKER`, // and mid-word, where the quoting is concatenation
	} {
		deniedEndToEnd(t, form)
	}
}

// A line continuation is deleted by the shell, not turned into a space:
// `to\<newline>uch x` is a touch, and reading it as `to uch x` matched nothing.
func TestLineContinuationInsideAWordIsRemoved(t *testing.T) {
	deniedEndToEnd(t, "to\\\nuch MARKER")
}

// A command word the shell builds at runtime is not a command word any reading
// here can spell. Emitting it as written and letting a rule fail to match it
// reported "no rule applies" for a command nobody could read.
func TestACommandWordBuiltAtRuntimeIsNotClear(t *testing.T) {
	for _, form := range []string{
		"C=touch; $C MARKER",
		"t${x}ouch MARKER",
		"t$@ouch MARKER",
		"eval \"$(echo touch) MARKER\"",
	} {
		deniedEndToEnd(t, form)
	}
}

// The other side of the rule above: a deny rule is a narrowing the user wrote
// about one command, and it must not start refusing everything else because a
// command has a substitution somewhere in its arguments.
func TestDenyRulesDoNotReachOrdinaryCommands(t *testing.T) {
	e := NewExecutor(t.TempDir(), ModeBypass)
	e.SetRules(nil, nil, []string{"bash(rm -rf /*)", "bash(git push*)", "bash(curl*)"})
	for _, cmd := range []string{
		"go build ./...",
		"make VERSION=$(git describe --tags) build",
		"git commit -m 'fix: submodule push handling'",
		"if [ -f go.mod ]; then go vet ./...; fi",
		"[ -d .git ] && git rev-parse HEAD",
		"[[ -n $HOME ]] && echo yes",
		"for f in *.go; do gofmt -l $f; done",
		"env FOO=$BAR go test ./...",
		"GOFLAGS=-mod=mod go build ./...",
		"go test ./... 2>&1 | tail -5",
		"npm run test -- --watch=false",
		"nice -n 10 make",
		"time go build ./...",
		"echo $HOME",
		"printf '%s\\n' \"$PWD\"",
	} {
		for _, seg := range segments(cmd) {
			if act := e.RuleFor("bash", seg); act != "" {
				t.Errorf("a deny rule reached %q (segment %q) as %s", cmd, seg, act)
			}
		}
	}
}

// Every path the tools judge is resolved, so the working directory they are
// judged against has to be. A directory reached through a symlink is ordinary
// — /tmp on macOS, ~/src -> /data/src, a home an image links elsewhere — and
// the unresolved comparison could never match there: each file in the project
// read as being outside it.
func TestSymlinkedWorkingDirectoryIsStillTheWorkingDirectory(t *testing.T) {
	real := t.TempDir()
	link := filepath.Join(t.TempDir(), "work")
	if err := os.Symlink(real, link); err != nil {
		t.Skip(err)
	}
	if err := os.WriteFile(filepath.Join(real, "main.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	e := NewExecutor(link, ModeAuto)

	// Control: a file genuinely outside still counts as outside, so the
	// assertion below is not passing because everything now counts as inside.
	outside := filepath.Join(t.TempDir(), "elsewhere.go")
	if e.inWorkArea(e.resolve(outside)) {
		t.Fatal("control: a path outside the tree was read as inside it")
	}

	raw, err := json.Marshal(map[string]string{"path": "main.go", "content": "package main\n"})
	if err != nil {
		t.Fatal(err)
	}
	asked := ""
	if _, ok := e.Approve("write_file", raw, func(tool, summary string) Decision {
		asked = tool
		return Decision{Allow: true}
	}); !ok {
		t.Fatal("an ordinary edit was refused")
	}
	if asked != "" {
		t.Errorf("auto mode prompted for an in-tree edit as %q", asked)
	}
}
