package secretenv

import (
	"strings"
	"testing"
)

func has(env []string, name string) bool {
	for _, kv := range env {
		if n, _, ok := strings.Cut(kv, "="); ok && n == name {
			return true
		}
	}
	return false
}

func TestScrubbedHidesProviderKeys(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "sk-secret")
	t.Setenv("OPENAI_API_KEY", "sk-other")
	t.Setenv("PATH_LIKE_NORMAL", "keep-me")

	env := Scrubbed()
	if has(env, "ANTHROPIC_API_KEY") || has(env, "OPENAI_API_KEY") {
		t.Fatal("provider key survived scrubbing")
	}
	if !has(env, "PATH_LIKE_NORMAL") {
		t.Fatal("ordinary variable was dropped")
	}
	if !has(env, "PATH") {
		t.Fatal("PATH must survive or tools break")
	}
}

func TestScrubbedHonorsConfiguredNames(t *testing.T) {
	t.Setenv("MY_COMPANY_TOKEN", "hunter2")
	SetExtra([]string{"my_company_token"}) // case-insensitive
	defer SetExtra(nil)

	if has(Scrubbed(), "MY_COMPANY_TOKEN") {
		t.Fatal("configured secret survived scrubbing")
	}
}
