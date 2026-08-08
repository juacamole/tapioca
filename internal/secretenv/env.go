// Package secretenv filters provider credentials out of the environments
// handed to subprocesses (shell tools, MCP servers).
package secretenv

import (
	"os"
	"strings"
)

// Tapioca's own provider credentials are worth more than anything the agent
// is working on, and no legitimate tool call needs them: a leaked key funds
// an attacker's inference, and injected instructions can print the
// environment as easily as any other command. Subprocesses therefore get a
// filtered copy.
var secretEnvNames = map[string]bool{
	"ANTHROPIC_API_KEY": true, "OPENAI_API_KEY": true, "GEMINI_API_KEY": true,
	"GOOGLE_API_KEY": true, "GROQ_API_KEY": true, "MISTRAL_API_KEY": true,
	"DEEPSEEK_API_KEY": true, "XAI_API_KEY": true, "OPENROUTER_API_KEY": true,
	"TOGETHER_API_KEY": true, "PERPLEXITY_API_KEY": true, "CEREBRAS_API_KEY": true,
	"AWS_SECRET_ACCESS_KEY": true, "AWS_SESSION_TOKEN": true,
	"AZURE_OPENAI_API_KEY": true, "HF_TOKEN": true,
}

var extraSecretEnv []string

// SetExtra adds user-configured variable names to the scrub list.
func SetExtra(names []string) { extraSecretEnv = names }

func isSecretEnv(name string) bool {
	if secretEnvNames[strings.ToUpper(name)] {
		return true
	}
	for _, n := range extraSecretEnv {
		if strings.EqualFold(n, name) {
			return true
		}
	}
	return false
}

// Scrubbed returns the process environment without provider credentials.
func Scrubbed() []string {
	src := os.Environ()
	out := make([]string, 0, len(src))
	for _, kv := range src {
		name, _, ok := strings.Cut(kv, "=")
		if ok && isSecretEnv(name) {
			continue
		}
		out = append(out, kv)
	}
	return out
}
