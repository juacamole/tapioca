package tools

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// realPath gave up after its link budget and returned a lexical path, which
// inWorkArea then approved while the kernel followed the last link out. A
// chain of in-tree relative links — all of which git stores and a clone
// reproduces — was enough.
func TestExhaustedSymlinkBudgetFailsClosed(t *testing.T) {
	e := execIn(t, ModeAuto)
	outside := t.TempDir()
	if r, err := filepath.EvalSymlinks(outside); err == nil {
		outside = r
	}
	victim := filepath.Join(outside, "authorized_keys")

	// L1 -> L2 -> … -> L80 -> the file outside, none of which exist.
	const n = 80
	for i := 1; i < n; i++ {
		from := filepath.Join(e.Cwd(), "L"+itoa(i))
		if err := os.Symlink("L"+itoa(i+1), from); err != nil {
			t.Skipf("symlinks unavailable: %v", err)
		}
	}
	if err := os.Symlink(victim, filepath.Join(e.Cwd(), "L"+itoa(n))); err != nil {
		t.Fatal(err)
	}

	got := e.resolve("L1")
	if e.inWorkArea(got) {
		t.Errorf("resolve = %q, which inWorkArea approved", got)
	}
	var asked []string
	e.CallDetailed(context.Background(), "write_file",
		args(t, map[string]string{"path": "L1", "content": "attacker"}),
		asker(Decision{Allow: false}, &asked))
	if _, err := os.Stat(victim); err == nil {
		t.Fatal("wrote outside the work area through a long symlink chain")
	}
}

// A # comment runs to the newline in sh, but the splitter modelled quotes and
// not comments — so a stray quote inside a comment flipped it into "quoted"
// for the rest of the command and swallowed the next line.
func TestCommentCannotSwallowTheNextCommand(t *testing.T) {
	for _, cmd := range []string{
		"echo hi #'\ntouch MARKER",
		"echo hi #\"\ntouch MARKER",
		"echo hi # it's fine\ntouch MARKER",
	} {
		e := execIn(t, ModeManual)
		e.SetBashPrefixes([]string{"echo"})
		marker := filepath.Join(e.Cwd(), "MARKER")
		var asked []string
		if _, _, err := e.Call(context.Background(), "bash",
			args(t, map[string]string{"command": strings.ReplaceAll(cmd, "MARKER", marker)}),
			asker(Decision{Allow: false}, &asked)); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(marker); err == nil {
			t.Errorf("%q ran unprompted under an echo grant", cmd)
		}
	}
}

// A comment must not stop the rest of the command being seen either. The
// splitter drops the comment and the line after it becomes its own segment,
// which is where the redirect is then caught.
func TestCommentsDoNotHideARedirect(t *testing.T) {
	segs := segments("echo hi #'\necho x > /tmp/f")
	if len(segs) != 2 {
		t.Fatalf("segments = %q, want the comment dropped and two commands", segs)
	}
	if escapes(segs[0]) {
		t.Errorf("the plain command %q was flagged", segs[0])
	}
	if !escapes(segs[1]) {
		t.Errorf("the redirect in %q went unnoticed", segs[1])
	}
	// An ordinary trailing comment still leaves one plain segment.
	if got := segments("echo hi # a note"); len(got) != 1 || got[0] != "echo hi" {
		t.Errorf("segments = %q, want [echo hi]", got)
	}
}

// normalized only looked at the first field, so anything a shell steps over
// before reaching the command hid it from a deny rule.
func TestDenyRuleSurvivesShellPrefixes(t *testing.T) {
	for _, cmd := range []string{
		"touch x", "FOO=1 touch x", "FOO=1 BAR=2 touch x", "! touch x",
		"( touch x )", "{ touch x; }", "(touch x)", `touch "x"`,
		"/usr/bin/touch x", `\touch x`, "touch\tx",
	} {
		e := execIn(t, ModeBypass)
		e.SetRules(nil, nil, []string{"bash(touch *)"})
		if got := e.ruleFor("bash", cmd); got != RuleDeny {
			t.Errorf("ruleFor(%q) = %q, want deny", cmd, got)
		}
	}
	// Quoting in a later word must not hide it either.
	e := execIn(t, ModeBypass)
	e.SetRules(nil, nil, []string{"bash(git push*)"})
	for _, cmd := range []string{`git push`, `git "push" --force`, `git 'push'`} {
		if got := e.ruleFor("bash", cmd); got != RuleDeny {
			t.Errorf("ruleFor(%q) = %q, want deny", cmd, got)
		}
	}
}

// An expansion in the first word means the first word is not the program.
func TestExpansionInTheCommandWordCountsAsAnEscape(t *testing.T) {
	for _, cmd := range []string{"touch$IFS/tmp/x", "$CMD arg", "ec$X ho hi"} {
		if !escapes(cmd) {
			t.Errorf("escapes(%q) = false", cmd)
		}
	}
	// A variable in an argument is ordinary and must still be allowed.
	if escapes("echo $HOME") {
		t.Error(`escapes("echo $HOME") = true; only the command word matters`)
	}
}

// A mention inlines the same bytes read_file would, into the same prompt.
func TestMentionBlockedMatchesTheReadGate(t *testing.T) {
	e := execIn(t, ModeManual)
	home := t.TempDir()
	if r, err := filepath.EvalSymlinks(home); err == nil {
		home = r
	}
	t.Setenv("HOME", home)
	key := filepath.Join(home, ".ssh", "id_ed25519")
	if err := os.MkdirAll(filepath.Dir(key), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(key, []byte("PRIVATE"), 0o600); err != nil {
		t.Fatal(err)
	}
	if !e.MentionBlocked(key) {
		t.Error("an absolute path to a private key was attachable")
	}
	if !e.MentionBlocked(filepath.Join(home, "notes.txt")) {
		t.Error("a path outside the working directory was attachable")
	}
	inTree := filepath.Join(e.Cwd(), "main.go")
	if err := os.WriteFile(inTree, []byte("package main"), 0o600); err != nil {
		t.Fatal(err)
	}
	if e.MentionBlocked(inTree) {
		t.Error("an ordinary file in the worktree was refused")
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}
