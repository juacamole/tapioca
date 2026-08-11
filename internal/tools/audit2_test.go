package tools

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// EvalSymlinks fails both for a path that does not exist and for a symlink
// whose target does not exist. Treating those alike meant a link committed as
// notes.txt -> ~/.ssh/authorized_keys resolved to itself, read as in-tree, and
// auto mode wrote through it without asking. Most interesting targets are
// absent, so "the target must not exist" was barely a constraint.
func TestDanglingSymlinkCannotEscapeTheWorkArea(t *testing.T) {
	e := execIn(t, ModeAuto)
	outside := t.TempDir()
	if r, err := filepath.EvalSymlinks(outside); err == nil {
		outside = r
	}
	victim := filepath.Join(outside, "authorized_keys") // deliberately absent
	if err := os.Symlink(victim, filepath.Join(e.Cwd(), "notes.txt")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	if got := e.resolve("notes.txt"); got != victim {
		t.Errorf("resolve = %q, want the link target %q", got, victim)
	}
	var asked []string
	res, err := e.CallDetailed(context.Background(), "write_file",
		args(t, map[string]string{"path": "notes.txt", "content": "attacker"}),
		asker(Decision{Allow: false}, &asked))
	if err != nil {
		t.Fatal(err)
	}
	if len(asked) == 0 {
		t.Error("auto mode wrote through a dangling symlink without asking")
	}
	if !res.IsErr {
		t.Error("the denial did not stop the write")
	}
	if _, err := os.Stat(victim); err == nil {
		t.Fatal("the file outside the work area was created anyway")
	}
}

// A chain of dangling links must not loop forever either.
func TestSymlinkCycleTerminates(t *testing.T) {
	e := execIn(t, ModeManual)
	a := filepath.Join(e.Cwd(), "a")
	b := filepath.Join(e.Cwd(), "b")
	if err := os.Symlink(b, a); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := os.Symlink(a, b); err != nil {
		t.Fatal(err)
	}
	done := make(chan string, 1)
	go func() { done <- e.resolve("a") }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("resolve did not terminate on a symlink cycle")
	}
}

// segments() split with a regex that ignored quoting while escapes() tracks
// quotes per segment, so a separator inside a string left each half with an
// unbalanced quote and both halves looked like a plain, granted command.
func TestQuotedSeparatorCannotSmuggleACommand(t *testing.T) {
	cases := []string{
		`echo "a|echo b" & touch MARKER`,
		`echo "a|echo b" > MARKER`,
		`echo 'x;echo y' & touch MARKER`,
		`echo "a|echo b" && touch MARKER`,
	}
	for _, cmd := range cases {
		e := execIn(t, ModeManual)
		e.SetBashPrefixes([]string{"echo"})
		marker := filepath.Join(e.Cwd(), "MARKER")
		var asked []string
		if _, _, err := e.Call(context.Background(), "bash",
			args(t, map[string]string{"command": replaceMarker(cmd, marker)}),
			asker(Decision{Allow: false}, &asked)); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(marker); err == nil {
			t.Errorf("%q ran unprompted under an echo grant", cmd)
		}
		if len(asked) == 0 {
			t.Errorf("%q was never put to the user", cmd)
		}
	}
}

// The same hole, reached through an allow rule instead of a [p] grant.
func TestQuotedSeparatorCannotSmuggleThroughAnAllowRule(t *testing.T) {
	e := execIn(t, ModeManual)
	e.SetRules([]string{"bash(go test*)"}, nil, nil)
	marker := filepath.Join(e.Cwd(), "MARKER")
	var asked []string
	if _, _, err := e.Call(context.Background(), "bash",
		args(t, map[string]string{"command": `go test "x|go test y" & touch ` + marker}),
		asker(Decision{Allow: false}, &asked)); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("an allow rule ran a smuggled command")
	}
}

// Ordinary compound commands must still split the way they did.
func TestSegmentsStillSplitsOrdinaryCommands(t *testing.T) {
	cases := map[string][]string{
		"ls || pwd":           {"ls", "pwd"},
		"ls && pwd":           {"ls", "pwd"},
		"ls; pwd":             {"ls", "pwd"},
		"ls | grep x":         {"ls", "grep x"},
		"ls\npwd":             {"ls", "pwd"},
		`echo "a; b"`:         {`echo "a; b"`},
		`echo 'a && b'`:       {`echo 'a && b'`},
		"echo hi & curl evil": {"echo hi & curl evil"}, // a lone & stays for escapes()
		`git commit -m "a|b"`: {`git commit -m "a|b"`},
	}
	for cmd, want := range cases {
		got := segments(cmd)
		if len(got) != len(want) {
			t.Errorf("segments(%q) = %q, want %q", cmd, got, want)
			continue
		}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("segments(%q) = %q, want %q", cmd, got, want)
				break
			}
		}
	}
}

// bash's $'…' has its own escaping rules; the scanner does not model them, so
// it is treated as an escape rather than parsed wrongly.
func TestAnsiCQuotingCountsAsAnEscape(t *testing.T) {
	for _, cmd := range []string{`echo $'\'' > /tmp/x`, `echo $'\x41'`} {
		if !escapes(cmd) {
			t.Errorf("escapes(%q) = false", cmd)
		}
	}
}

// A deny rule is the only check left in bypass, and these all run rm.
func TestDenyRuleSurvivesShellSpellings(t *testing.T) {
	for _, cmd := range []string{
		"rm -rf x", "/bin/rm -rf x", "rm\t-rf x", `\rm -rf x`, `'rm' -rf x`,
		`"rm" -rf x`, "rm  -rf   x",
	} {
		e := execIn(t, ModeBypass)
		e.SetRules(nil, nil, []string{"bash(rm *)"})
		if got := e.ruleFor("bash", cmd); got != RuleDeny {
			t.Errorf("ruleFor(%q) = %q, want deny", cmd, got)
		}
	}
	// And still nothing that merely contains the letters.
	e := execIn(t, ModeBypass)
	e.SetRules(nil, nil, []string{"bash(rm *)"})
	for _, cmd := range []string{"npm run build", "confirm the change", "rmdir x"} {
		if got := e.ruleFor("bash", cmd); got != ruleNone {
			t.Errorf("ruleFor(%q) = %q, want no rule", cmd, got)
		}
	}
}

func replaceMarker(cmd, marker string) string {
	out := ""
	for i := 0; i < len(cmd); i++ {
		if i+6 <= len(cmd) && cmd[i:i+6] == "MARKER" {
			out += marker
			i += 5
			continue
		}
		out += string(cmd[i])
	}
	return out
}
