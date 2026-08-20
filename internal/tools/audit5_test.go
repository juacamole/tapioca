package tools

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// ranUnprompted runs command end to end with a blanket grant on word and
// reports whether the marker appeared without anyone being asked.
func ranUnprompted(t *testing.T, dir, word, command string) (ran, asked bool) {
	t.Helper()
	e := NewExecutor(dir, ModeManual)
	e.SetBashPrefixes([]string{word})
	raw, err := json.Marshal(map[string]string{"command": command})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := e.Call(context.Background(), "bash", raw, func(string, string) Decision {
		asked = true
		return Decision{Allow: false}
	}); err != nil {
		t.Fatal(err)
	}
	return false, asked
}

// marked runs command under a grant and reports whether the marker exists.
func marked(t *testing.T, dir, word, command, marker string) bool {
	t.Helper()
	_, _ = ranUnprompted(t, dir, word, command)
	_, err := os.Stat(marker)
	return err == nil
}

// unquoteWord is not a shell. `git $"-c"` reaches git as -c because $"..." is
// locale translation, and `git -?` reaches it as -c when the tree holds a file
// the repository named "-c" — which an extracted tarball chooses. Both walked
// past the exec-flag check and ran under a blanket grant on git.
func TestExpansionCannotHideAnExecFlag(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	for _, form := range []string{`git $"-c" alias.p='!touch MARKER' p`, `git -? alias.p='!touch MARKER' p`} {
		dir := t.TempDir()
		// A repository is allowed to contain a file named "-c".
		if err := os.WriteFile(filepath.Join(dir, "-c"), nil, 0o644); err != nil {
			t.Fatal(err)
		}
		marker := filepath.Join(dir, "PWNED")
		// Control: the plain spelling is refused, so the assertion below is not
		// passing because the attack simply does not work in this shell.
		plain := `git -c alias.p='!touch ` + marker + `' p`
		if _, asked := ranUnprompted(t, dir, "git", plain); !asked {
			t.Fatal("control failed: plain `git -c` was covered by the grant")
		}
		if marked(t, dir, "git", strings.ReplaceAll(form, "MARKER", marker), marker) {
			t.Errorf("a git grant covered %q", form)
		}
	}
}

// A word the shell rewrites cannot be read, so it counts as reaching for an
// exec flag — but only for a command that has any, or every glob would prompt.
func TestExpandedArgumentsOnlyMatterForExecCapableCommands(t *testing.T) {
	e := NewExecutor(t.TempDir(), ModeManual)
	e.SetBashPrefixes([]string{"ls", "cat", "grep", "git"})
	for _, seg := range []string{"ls *.go", "cat src/*.txt", "grep -r x ~", "git status", "git diff HEAD"} {
		if !e.segmentAllowed(seg) {
			t.Errorf("grant stopped covering the ordinary %q", seg)
		}
	}
	for _, seg := range []string{"git $\"-c\" x", "git -? x", "git ${F} status", "git *"} {
		if e.segmentAllowed(seg) {
			t.Errorf("a git grant covered %q, whose arguments the shell rewrites", seg)
		}
	}
}

// Some git subcommands run a program the caller names without any flag the
// scan would recognise. Each was verified to run with output on a pipe.
func TestGitExecSubcommandsAreNotCoveredByAGrant(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo := func(t *testing.T) string {
		dir := t.TempDir()
		for _, a := range [][]string{
			{"init", "-q", "."}, {"config", "user.email", "t@e"}, {"config", "user.name", "t"},
		} {
			if out, err := exec.Command("git", append([]string{"-C", dir}, a...)...).CombinedOutput(); err != nil {
				t.Skipf("git setup: %v %s", err, out)
			}
		}
		for i, body := range []string{"a\n", "b\n"} {
			if err := os.WriteFile(filepath.Join(dir, "f"), []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
			_ = exec.Command("git", "-C", dir, "add", "f").Run()
			_ = exec.Command("git", "-C", dir, "commit", "-qm", string(rune('a'+i))).Run()
		}
		return dir
	}
	for _, form := range []string{
		`git difftool -y -x 'touch MARKER --' HEAD~1 HEAD`,
		`git rebase -x 'touch MARKER' HEAD~1`,
		`git filter-branch -f --tree-filter 'touch MARKER' HEAD`,
		`git bisect run touch MARKER`,
	} {
		dir := repo(t)
		marker := filepath.Join(dir, "MARKER")
		cmd := strings.ReplaceAll(form, "MARKER", marker)
		// Control: run it outside the gate to be sure this git build does what
		// the test says it does.
		ctl := exec.Command("sh", "-c", cmd)
		ctl.Dir = dir
		// filter-branch sleeps ten seconds on its deprecation warning.
		ctl.Env = append(os.Environ(), "FILTER_BRANCH_SQUELCH_WARNING=1")
		_ = ctl.Run()
		if _, err := os.Stat(marker); err != nil {
			t.Logf("this git does not run the program for %q; only the gate is proven", form)
		} else if err := os.Remove(marker); err != nil {
			t.Fatal(err)
		}
		if marked(t, dir, "git", cmd, marker) {
			t.Errorf("a git grant covered %q", form)
		}
	}
}

// A deny rule is documented to hold in bypass, where it is the only check left.
// These spellings all reach the shell as touch and matched no rule for touch:
// bash's other assignment prefixes, its $"..." quoting, the keywords that
// introduce a command, eval, and a wrapper option whose value was mistaken for
// the command.
func TestDenyRuleHoldsAcrossMoreSpellings(t *testing.T) {
	for _, form := range []string{
		`FOO+=1 touch MARKER`,
		`A[0]=1 touch MARKER`,
		`$"touch" MARKER`,
		`if true; then touch MARKER; fi`,
		`if touch MARKER; then :; fi`,
		`until touch MARKER; do :; done`,
		`while :; do touch MARKER; break; done`,
		`eval touch MARKER`,
		`env -u FOO touch MARKER`,
		`env -i touch MARKER`,
		`timeout -s KILL 5 touch MARKER`,
		`sudo -n -u root touch MARKER`,
	} {
		dir := t.TempDir()
		marker := filepath.Join(dir, "MARKER")
		cmd := strings.ReplaceAll(form, "MARKER", marker)
		// Control: without the rule the command really does create the marker,
		// so a pass below cannot come from the command simply not working.
		ctl := exec.Command("sh", "-c", cmd)
		ctl.Dir = dir
		_ = ctl.Run()
		if _, err := os.Stat(marker); err != nil {
			t.Logf("this shell does not run %q; only the rule is proven", form)
		} else if err := os.Remove(marker); err != nil {
			t.Fatal(err)
		}

		e := NewExecutor(dir, ModeBypass)
		e.SetRules(nil, nil, []string{"bash(touch*)"})
		raw, err := json.Marshal(map[string]string{"command": cmd})
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := e.Call(context.Background(), "bash", raw, func(string, string) Decision {
			return Decision{Allow: true}
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(marker); err == nil {
			t.Errorf("%q ran despite a deny rule", form)
		}
	}
}

// Widening the normalizer must not turn an innocent command into a denial: the
// word after a wrapper's operand is the command, and nothing past it is read.
func TestNormalizerDoesNotOverreach(t *testing.T) {
	e := NewExecutor(t.TempDir(), ModeBypass)
	e.SetRules(nil, nil, []string{"bash(rm*)"})
	for _, cmd := range []string{
		"timeout 5 git log --grep rm",
		"env FOO=1 git commit -m 'drop rm'",
		"go test ./internal/rm",
	} {
		if got := e.ruleFor("bash", cmd); got == RuleDeny {
			t.Errorf("ruleFor(%q) = deny; the rule was for the rm command", cmd)
		}
	}
}
