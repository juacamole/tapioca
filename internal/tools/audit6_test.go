package tools

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A lone & chains a second command that segments() deliberately does not split
// on, so the whole thing arrives at the rules as one segment whose first word
// belongs to the first command. escapes() knows about that & and refuses to
// let a grant cover it — but a deny rule was matched against the joined text
// and against normalizedForms of it, both of which read the segment as an
// `echo`, so the denied command after the & matched nothing. Under bypass,
// where nothing prompts, it simply ran; and a denial that does not hold under
// bypass is not a denial.
func TestDenyReachesPastABackgroundedCommand(t *testing.T) {
	for _, mode := range []string{ModeBypass, ModeAuto} {
		e := execIn(t, mode)
		e.SetRules(nil, nil, []string{"bash(touch*)"})
		var asked []string
		allow := asker(Decision{Allow: true}, &asked)

		// Control: the denied command on its own is stopped, so a marker that
		// appears below appeared because of the &.
		if _, err := os.Stat(filepath.Join(e.Cwd(), "control")); err == nil {
			t.Fatal("stale marker")
		}
		out, isErr, err := e.Call(context.Background(), "bash",
			args(t, map[string]string{"command": "touch control"}), allow)
		if err != nil {
			t.Fatal(err)
		}
		if !isErr {
			t.Fatalf("control failed: the deny rule does not stop the plain command: %q", out)
		}

		if _, _, err := e.Call(context.Background(), "bash",
			args(t, map[string]string{"command": "true & touch pwned"}), allow); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(filepath.Join(e.Cwd(), "pwned")); err == nil {
			t.Errorf("%s: a denied command ran when a lone & put it second", mode)
		}
	}
}

// The same shape twice more: a command substitution and a shell's -c argument
// both run a program that is not the first word of the segment, so a deny rule
// read the segment as an `echo` or a `sh` and the program inside ran. Under
// bypass nothing prompts, so the rule was the only thing there was.
func TestDenyReachesInsideSubstitutionsAndShells(t *testing.T) {
	for _, cmd := range []string{
		"echo $(touch pwned)",
		"echo `touch pwned`",
		"echo \"$(touch pwned)\"",
		"cat <(touch pwned)",
		"sh -c 'touch pwned'",
		"bash -c \"touch pwned\"",
		"echo $(true && touch pwned)",
	} {
		e := execIn(t, ModeBypass)
		e.SetRules(nil, nil, []string{"bash(touch*)"})
		var asked []string
		allow := asker(Decision{Allow: true}, &asked)
		if _, isErr, err := e.Call(context.Background(), "bash",
			args(t, map[string]string{"command": "touch control"}), allow); err != nil || !isErr {
			t.Fatalf("control failed: the deny rule does not stop a plain touch (isErr=%v err=%v)", isErr, err)
		}
		if _, _, err := e.Call(context.Background(), "bash",
			args(t, map[string]string{"command": cmd}), allow); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(filepath.Join(e.Cwd(), "pwned")); err == nil {
			t.Errorf("a denied command ran from inside: %s", cmd)
		}
	}
}

// And the everyday commands that carry an &, a substitution or a shell for
// reasons that are not chaining: a deny rule for one program must not start
// matching another because of one. Widening what a rule reads is only ever
// worth it if this list keeps passing.
func TestWiderReadingDoesNotOverreach(t *testing.T) {
	e := execIn(t, ModeBypass)
	e.SetRules(nil, nil, []string{"bash(rm*)"})
	var asked []string
	allow := asker(Decision{Allow: true}, &asked)
	for _, cmd := range []string{
		"echo hi 2>&1",
		"echo hi &>/dev/null",
		"echo 'a & rm -rf /'",
		"git commit -m \"tidy & rm helpers\"",
		"echo $(date)",
		"echo \"branch: $(git rev-parse --abbrev-ref HEAD)\"",
		"sh -c 'echo hello'",
		"echo 'rm is a word in this string'",
		"grep -r rm .",
		"go test ./... 2>&1",
	} {
		out, isErr, err := e.Call(context.Background(), "bash",
			args(t, map[string]string{"command": cmd}), allow)
		if err != nil {
			t.Fatal(err)
		}
		if isErr && strings.Contains(out, "permission rule") {
			t.Errorf("an unrelated command was denied: %q -> %s", cmd, out)
		}
	}
}

// A blanket grant is offered on the command a user pressed [p] on, and every
// later command with that first word runs unread. So the grant is worth
// whatever the *most* dangerous spelling of that command is — and for git and
// go, the dangerous spelling is not a flag on the command being run but a write
// into the user's own global configuration, which turns every later invocation
// on the machine into arbitrary execution. That outlives the tree, the session
// and the grant.
//
// The one-shot spellings are already refused, which is what makes these two
// worth having: the same power, reached by a form the list did not name.
func TestBlanketGrantCannotWriteExecutionIntoGlobalConfig(t *testing.T) {
	cases := []struct {
		grantedOn string
		control   string   // the refused one-shot spelling of the same power
		attacks   []string // must be refused too
		ordinary  []string // must stay granted
	}{{
		grantedOn: "git status",
		control:   "git -c alias.st=!touch /tmp/pwned st",
		attacks: []string{
			"git config --global alias.st !touch /tmp/pwned",
			"git config --global core.fsmonitor /tmp/pwn.sh",
			"git config --global core.hooksPath /tmp/hooks",
			"git config --global credential.helper !cat >/tmp/creds",
		},
		ordinary: []string{
			"git status", "git diff --stat", "git log -n 5 --oneline",
			"git add -A", "git commit -m fix-config", "git push origin main",
		},
	}, {
		grantedOn: "go build ./...",
		control:   "go build -toolexec=/tmp/wrap.sh ./...",
		attacks: []string{
			"go env -w GOFLAGS=-toolexec=/tmp/wrap.sh",
			"go env -w GOFLAGS='-toolexec=/tmp/wrap.sh'",
			"go env --w GOFLAGS=-toolexec=/tmp/wrap.sh",
			// Verified to run the program: go's flag parser takes --flag for
			// every -flag, so a list written with one dash matched neither.
			"go build --toolexec=/tmp/wrap.sh ./...",
			"go test --exec /tmp/wrap.sh ./...",
			"go vet --vettool=/tmp/wrap.sh ./...",
		},
		ordinary: []string{
			"go build ./...", "go test ./...", "go vet ./...",
			"go env GOPATH", "go env", "go mod tidy", "go fmt ./...",
			// -w is a linker flag long before it is `go env -w`.
			"go build -ldflags \"-w -s\" ./cmd/tapio",
			"go build -ldflags=-w ./...",
		},
	}, {
		// The two lists are shared, so a change to how a flag is read has to be
		// checked against the other commands on them.
		grantedOn: "make test",
		control:   "make -f /tmp/evil.mk",
		attacks:   []string{"make --file=/tmp/evil.mk", "make --eval=$(shell id)"},
		ordinary:  []string{"make", "make test", "make -j4 build", "make install"},
	}, {
		grantedOn: "cargo build",
		control:   "cargo run",
		attacks:   nil,
		ordinary:  []string{"cargo build", "cargo test", "cargo check --all-features"},
	}}

	for _, tc := range cases {
		e := NewExecutor(t.TempDir(), ModeManual)
		if !PrefixGrantable(tc.grantedOn) {
			t.Fatalf("control failed: [p] is not offered for %q, so there is no grant to escape", tc.grantedOn)
		}
		e.SetBashPrefixes([]string{PrefixSuggestion(tc.grantedOn)})
		if e.segmentAllowed(tc.control) {
			t.Fatalf("control failed: %q was already granted, so the cases below prove nothing", tc.control)
		}
		for _, seg := range tc.attacks {
			if e.segmentAllowed(seg) {
				t.Errorf("a grant on %q ran unread: %s", PrefixSuggestion(tc.grantedOn), seg)
			}
		}
		for _, seg := range tc.ordinary {
			if !e.segmentAllowed(seg) {
				t.Errorf("a grant on %q stopped covering an everyday command: %s",
					PrefixSuggestion(tc.grantedOn), seg)
			}
		}
	}
}

// What those two commands are worth, run for real: an alias written into the
// global config is a shell command, and the next git invocation runs it. The
// same grant covers both halves, so nothing is asked in between.
func TestGlobalGitConfigReallyExecutes(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	home := t.TempDir()
	marker := filepath.Join(home, "pwned")
	run := func(args ...string) {
		t.Helper()
		c := exec.Command("git", args...)
		c.Dir = home
		c.Env = append(os.Environ(), "HOME="+home, "GIT_CONFIG_GLOBAL="+filepath.Join(home, ".gitconfig"))
		_ = c.Run()
	}
	run("config", "--global", "alias.st", "!touch "+marker)
	run("init", "-q", ".")
	run("st")
	if _, err := os.Stat(marker); err != nil {
		t.Skipf("git did not run the alias here, so this machine cannot show the impact: %v", err)
	}
}

// The command is written by the model, so the readings above have to terminate
// on input chosen to make them not: nesting is bounded, and a run that would
// once have gone as deep as the string is long returns instead of exhausting
// the stack.
func TestDeeplyNestedSubstitutionTerminates(t *testing.T) {
	cmd := "echo " + strings.Repeat("$(", 20000) + "touch x" + strings.Repeat(")", 20000)
	done := make(chan bool, 1)
	go func() {
		_ = normalizedForms(cmd)
		done <- true
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("normalizedForms did not finish on a deeply nested substitution")
	}
}
