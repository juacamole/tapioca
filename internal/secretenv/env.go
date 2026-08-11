// Package secretenv filters provider credentials out of the environments
// handed to subprocesses (shell tools, MCP servers).
package secretenv

import (
	"os"
	"strings"
	"sync"
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
	"AWS_SECRET_ACCESS_KEY": true, "AWS_SESSION_TOKEN": true, "AWS_ACCESS_KEY_ID": true,
	"AZURE_OPENAI_API_KEY": true, "HF_TOKEN": true,
	// Tapioca reads this one itself for Vertex, so it is a live bearer token
	// sitting in the environment of every tool call.
	"GOOGLE_ACCESS_TOKEN": true, "GOOGLE_APPLICATION_CREDENTIALS": true,
	"ANTHROPIC_AUTH_TOKEN": true, "GH_TOKEN": true, "GITHUB_TOKEN": true,
}

var (
	extraMu        sync.RWMutex
	extraSecretEnv []string
)

// SetExtra adds user-configured variable names to the scrub list. It runs
// again on every config reload, while tool calls may be scrubbing on other
// goroutines, so the list is guarded — a torn read here fails open.
func SetExtra(names []string) {
	extraMu.Lock()
	extraSecretEnv = append([]string(nil), names...)
	extraMu.Unlock()
}

func isSecretEnv(name string) bool {
	if secretEnvNames[strings.ToUpper(name)] {
		return true
	}
	extraMu.RLock()
	defer extraMu.RUnlock()
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
