package config

import (
	"testing"

	"tapioca/internal/secretenv"
)

func hasEnv(env []string, name string) bool {
	for _, kv := range env {
		if len(kv) > len(name) && kv[:len(name)] == name && kv[len(name)] == '=' {
			return true
		}
	}
	return false
}

// A variable holds a credential because the config says to read it, not
// because its name is in a table in the source. api_key_env and the ${VAR} an
// mcp header expands were handed intact to every stdio MCP server, every
// language server and every bash call.
func TestSecretEnvNamesFollowTheConfig(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "control")
	t.Setenv("EXPLICIT", "x")
	t.Setenv("MY_GATEWAY_KEY", "sk-custom")
	t.Setenv("MCP_TOKEN", "bearer")
	t.Setenv("AI_GATEWAY_API_KEY", "sk-vercel")
	t.Setenv("ORDINARY_VAR", "keep")

	c := Default()
	c.SecretEnv = []string{"EXPLICIT"}
	c.Providers = map[string]ProviderConfig{
		"gw":   {Type: "openai", APIKeyEnv: "MY_GATEWAY_KEY"},
		"none": {Type: "openai"}, // no api_key_env: must not add an empty name
	}
	c.MCP = []MCPServerConfig{{
		Name:    "s",
		Headers: map[string]string{"Authorization": "Bearer ${MCP_TOKEN}", "X-Plain": "literal"},
	}}

	// Control: the fixed table already covers this one, so a pass below is not
	// the scrubber refusing to hand over anything at all.
	if hasEnv(secretenv.Scrubbed(), "ANTHROPIC_API_KEY") {
		t.Fatal("control failed: a listed key was not scrubbed")
	}
	secretenv.SetExtra(c.SecretEnvNames())
	defer secretenv.SetExtra(nil)

	env := secretenv.Scrubbed()
	for _, name := range []string{"EXPLICIT", "MY_GATEWAY_KEY", "MCP_TOKEN", "AI_GATEWAY_API_KEY"} {
		if hasEnv(env, name) {
			t.Errorf("%s reaches every subprocess", name)
		}
	}
	if !hasEnv(env, "ORDINARY_VAR") {
		t.Error("an ordinary variable was dropped")
	}
}
