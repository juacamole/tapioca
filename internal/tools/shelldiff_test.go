package tools

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Every round of this audit found another input where the gate's idea of a
// command and the shell's disagreed. The gate cannot be proven right by
// reading it, so this asks the real shell: for each command, does anything run
// that the user was never shown?
//
// The rule under test is the one SECURITY.md states — a granted word covers a
// segment that does nothing but run that word — so the check is simply: with a
// grant on "echo" and every prompt denied, no marker file may appear.
func TestNothingRunsThatWasNotApproved(t *testing.T) {
	cases := []string{
		// The plain forms the grant is for.
		`echo hi`,
		`echo "a b"`,
		`echo 'a b'`,
		// Separators, quoted and not.
		`echo hi; touch MARKER`,
		`echo hi && touch MARKER`,
		`echo hi || touch MARKER`,
		`echo hi | touch MARKER`,
		`echo hi & touch MARKER`,
		"echo hi\ntouch MARKER",
		`echo "a;b" ; touch MARKER`,
		`echo "a|b" & touch MARKER`,
		// Comments, and the word-boundary cases around them.
		"echo hi #'\ntouch MARKER",
		"echo a\\ #x; touch MARKER",
		"echo a\\\t#x; touch MARKER",
		"echo hi#not-a-comment; touch MARKER",
		"echo 'x #y'; touch MARKER",
		// Quoting that has bitten this scanner before.
		`echo 'x\' > MARKER`,
		`echo $'\'' ; touch MARKER`,
		`echo $'\x41' ; touch MARKER`,
		// Substitution and redirection.
		"echo $(touch MARKER)",
		"echo `touch MARKER`",
		`echo hi > MARKER`,
		`echo hi >> MARKER`,
		`> MARKER echo hi`,
		`echo ${x} ; touch MARKER`,
		// Wrappers and grouping.
		`(touch MARKER)`,
		`{ touch MARKER; }`,
		`! touch MARKER`,
		`FOO=1 touch MARKER`,
		`env touch MARKER`,
		`eval touch\ MARKER`,
	}

	for _, cmd := range cases {
		e := execIn(t, ModeManual)
		e.SetBashPrefixes([]string{"echo"})
		marker := filepath.Join(e.Cwd(), "MARKER")
		full := strings.ReplaceAll(cmd, "MARKER", marker)

		var asked []string
		if _, _, err := e.Call(context.Background(), "bash",
			args(t, map[string]string{"command": full}),
			asker(Decision{Allow: false}, &asked)); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(marker); err == nil {
			t.Errorf("%q ran with the user declining every prompt (shown: %q)", cmd, asked)
			os.Remove(marker)
		}
	}
}

// The other half: the grant has to still be worth having. A plain granted
// command must run without a prompt, or the gate is just an obstacle.
func TestGrantedCommandsStillRunUnprompted(t *testing.T) {
	for _, cmd := range []string{
		`echo hi`,
		`echo "a b"`,
		`echo 'a; b'`,
		`echo a#b`,
		`echo hi # a trailing note`,
	} {
		e := execIn(t, ModeManual)
		e.SetBashPrefixes([]string{"echo"})
		var asked []string
		if _, _, err := e.Call(context.Background(), "bash",
			args(t, map[string]string{"command": cmd}),
			asker(Decision{Allow: false}, &asked)); err != nil {
			t.Fatal(err)
		}
		if len(asked) != 0 {
			t.Errorf("%q prompted despite an echo grant: %q", cmd, asked)
		}
	}
}

// A deny rule is documented to hold in bypass, where it is the only check
// left, so the same list is run against one.
func TestDenyRuleHoldsAcrossSpellings(t *testing.T) {
	cases := []string{
		`touch MARKER`,
		`/usr/bin/touch MARKER`,
		`\touch MARKER`,
		`'touch' MARKER`,
		`"touch" MARKER`,
		`$'touch' MARKER`,
		"touch\tMARKER",
		`FOO=1 touch MARKER`,
		`FOO=1 BAR=2 touch MARKER`,
		`! touch MARKER`,
		`(touch MARKER)`,
		`{ touch MARKER; }`,
		`>/dev/null touch MARKER`,
		`env touch MARKER`,
		`command touch MARKER`,
		`nice touch MARKER`,
	}
	for _, cmd := range cases {
		e := execIn(t, ModeBypass)
		e.SetRules(nil, nil, []string{"bash(touch*)"})
		marker := filepath.Join(e.Cwd(), "MARKER")
		full := strings.ReplaceAll(cmd, "MARKER", marker)
		if _, _, err := e.Call(context.Background(), "bash",
			args(t, map[string]string{"command": full}),
			asker(Decision{Allow: true}, new([]string))); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(marker); err == nil {
			t.Errorf("%q ran despite a deny rule in bypass mode", cmd)
			os.Remove(marker)
		}
	}
}
