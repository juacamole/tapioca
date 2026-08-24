package tools

import "testing"

// A blanket grant is keyed on one word and matched by exact equality, so the
// word has to be a literal command name. "FOO=1 bash -c x" offered a grant on
// "FOO=1", and that grant then cleared every command written with the same
// assignment in front of it — including one the user never saw.
func TestAssignmentIsNotAGrantableWord(t *testing.T) {
	if got := PrefixSuggestion("FOO=1 bash -c id"); got != "" {
		t.Errorf("PrefixSuggestion = %q, want no suggestion", got)
	}
	e := NewExecutor(t.TempDir(), "manual")
	e.SetBashPrefixes([]string{"FOO=1"})
	if e.segmentAllowed("FOO=1 rm -rf /") {
		t.Error("an assignment granted as a word cleared an unrelated command")
	}
}

// Quoting the name changes nothing about what runs, but it did walk past the
// exec-capable check: "'b'ash -c x" was offered as a blanket grant.
func TestQuotedInterpreterIsNotGrantable(t *testing.T) {
	for _, c := range []string{
		`'b'ash -c id`, `\bash -c id`, `b""ash -c id`, `"bash" -c id`,
		`(bash -c id)`, `! bash -c id`, `{ bash -c id; }`,
	} {
		if PrefixGrantable(c) {
			t.Errorf("%q offered as a blanket grant", c)
		}
	}
}

// The plain forms must stay grantable, or the shortcut is worthless.
func TestOrdinaryCommandsStayGrantable(t *testing.T) {
	for _, c := range []string{
		"ls -la", "cat f.txt", "grep -r x .", "rg foo", "/usr/bin/ls",
		"git status", "git diff HEAD", "go test ./...", "make build",
		// The glob is quoted: an unquoted one in a command that has exec flags
		// is expanded against a directory the repository wrote, so what find
		// ends up being handed is not what anybody read.
		"find . -name '*.go'", "tar -czf a.tgz .", "npm test",
	} {
		if !PrefixGrantable(c) {
			t.Errorf("%q should be grantable", c)
		}
	}
}

// Each of these names a program to run in its arguments, verified against the
// real binary with output on a pipe. The command stays grantable; this exact
// use of it does not.
func TestExecFlagsAreNotCoveredByAGrant(t *testing.T) {
	cases := []struct{ word, segment string }{
		{"git", "git -c alias.x=!id x"},
		{"git", "git '-c' alias.x=!id x"},
		{"git", "git --exec-path=/tmp/evil status"},
		{"find", "find . -exec id {} ;"},
		{"find", "find . -execdir id {} ;"},
		{"go", "go run x.go"},
		{"go", "go generate ./..."},
		{"make", "make -f /tmp/evil"},
		{"tar", "tar --use-compress-program=id -cf /dev/null ."},
		{"rsync", "rsync -e id a b"},
		{"npm", "npm exec evil"},
	}
	for _, c := range cases {
		if PrefixGrantable(c.segment) {
			t.Errorf("%q offered as a blanket grant", c.segment)
		}
		// The half that matters: the grant already exists and the command
		// arrives later, with nobody reading it.
		e := NewExecutor(t.TempDir(), "manual")
		e.SetBashPrefixes([]string{c.word})
		if e.segmentAllowed(c.segment) {
			t.Errorf("a %q grant covered %q", c.word, c.segment)
		}
	}
}

// The grant still has to do its job for the ordinary case it was given for.
func TestGrantStillCoversTheOrdinaryCase(t *testing.T) {
	e := NewExecutor(t.TempDir(), "manual")
	e.SetBashPrefixes([]string{"git", "go"})
	for _, seg := range []string{"git status", "git diff HEAD", "go test ./...", "go build"} {
		if !e.segmentAllowed(seg) {
			t.Errorf("grant did not cover %q", seg)
		}
	}
}
