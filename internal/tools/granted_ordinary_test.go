package tools

import "testing"

// Everyday commands must still be covered by the grant the user gave.
func TestOrdinaryCommandsStayCoveredByAGrant(t *testing.T) {
	e := NewExecutor(t.TempDir(), "manual")
	e.SetBashPrefixes([]string{"git", "go", "ls", "grep", "find", "npm", "cargo", "make"})
	for _, seg := range []string{
		"git status", "git diff HEAD", "git log --oneline -5", "git add -A",
		"git commit -m fix", "git push origin main",
		"go build ./...", "go test ./internal/tools/", "go vet ./...",
		"ls -la", "grep -rn foo .", "find . -name '*.go'", "find . -type f",
		"npm test", "npm run build", "cargo build", "make build",
	} {
		if !e.segmentAllowed(seg) {
			t.Errorf("grant no longer covers %q", seg)
		}
	}
}

// The widened deny matching must not start denying unrelated commands.
func TestDenyRulesDoNotReachUnrelatedCommands(t *testing.T) {
	e := NewExecutor(t.TempDir(), "bypass")
	e.SetRules(nil, nil, []string{"bash(rm*)", "bash(touch*)"})
	for _, seg := range []string{
		"git log --grep rm", "echo remove", "grep -r touch .",
		"ls -la", "go test ./...", "cat rm.txt", "git commit -m 'rm old files'",
	} {
		if act := e.ruleFor("bash", seg); act == RuleDeny {
			t.Errorf("%q was denied by a rule for rm/touch", seg)
		}
	}
	for _, seg := range []string{"rm -rf /", "touch x", "/bin/rm x", "'rm' -rf x"} {
		if act := e.ruleFor("bash", seg); act != RuleDeny {
			t.Errorf("%q was NOT denied, got %q", seg, act)
		}
	}
}
