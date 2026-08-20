package tools

import (
	"strings"
	"testing"
	"time"
)

// Reading inside substitutions must not start denying ordinary commands that
// merely mention the denied word, or contain an unrelated substitution.
func TestSubstitutionReadingDoesNotOverreach(t *testing.T) {
	e := NewExecutor(t.TempDir(), "bypass")
	e.SetRules(nil, nil, []string{"bash(rm*)", "bash(touch*)", "bash(curl*)"})
	for _, seg := range []string{
		"echo $(date)",
		"git commit -m \"$(cat msg.txt)\"",
		"cd $(git rev-parse --show-toplevel)",
		"echo $(ls | wc -l)",
		"go test ./... 2>&1 | tail -5",
		"grep -rn touch .",
		"git log --grep rm --oneline",
		"echo \"remove the rm call\"",
		"tar -czf a.tgz $(ls)",
	} {
		if e.ruleFor("bash", seg) == RuleDeny {
			t.Errorf("denied an ordinary command: %q", seg)
		}
	}
}

// The bypasses themselves must be denied.
func TestSubstitutionBypassesAreDenied(t *testing.T) {
	e := NewExecutor(t.TempDir(), "bypass")
	e.SetRules(nil, nil, []string{"bash(touch*)"})
	for _, seg := range []string{
		"true & touch pwned",
		"echo $(touch pwned)",
		"echo `touch pwned`",
		"cat <(touch pwned)",
		"sh -c 'touch pwned'",
		"bash -c \"touch pwned\"",
	} {
		if e.ruleFor("bash", seg) != RuleDeny {
			t.Errorf("NOT denied: %q", seg)
		}
	}
}

// A hostile model can emit arbitrarily deep nesting; matching must terminate.
func TestDeepNestingTerminates(t *testing.T) {
	e := NewExecutor(t.TempDir(), "bypass")
	e.SetRules(nil, nil, []string{"bash(touch*)"})
	deep := strings.Repeat("$(", 20000) + "touch x" + strings.Repeat(")", 20000)
	done := make(chan string, 1)
	go func() { done <- e.ruleFor("bash", deep) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("deny matching did not terminate on deep nesting")
	}
}
