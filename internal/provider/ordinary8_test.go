package provider

import (
	"strings"
	"testing"

	"tapioca/internal/config"
)

// Ordinary addresses and ordinary regions. Nothing here is an attacker's
// value; each one is what a real setup contains.

// A model server on the LAN is how a household or an office runs ollama,
// llama.cpp, vLLM or LM Studio: one box with the GPU, plain http, no
// certificate. It is refused, and this pins that rather than calling it a
// mistake: the address decides where the prompts and the credential go, and
// http sends both across the network in the clear. The cost is real — there is
// no way to configure that box through the setup form — and the message has to
// keep saying what the alternative is, or the refusal is unactionable.
//
// The rule predates every round of this audit (it arrived with the custom
// provider itself); it is recorded here because it is the kind of refusal a
// later round would otherwise "discover" as a regression.
func TestOrdinaryLANModelServerAddressIsRefusedWithARemedy(t *testing.T) {
	for _, base := range []string{
		"http://192.168.1.50:11434",
		"http://10.0.0.7:8080/v1",
		"http://gpu-box.lan:11434",
	} {
		err := CheckBaseURL(base)
		if err == nil {
			t.Errorf("CheckBaseURL(%q) accepted plain http off this machine", base)
			continue
		}
		if !strings.Contains(err.Error(), "https") {
			t.Errorf("CheckBaseURL(%q) refused without naming the remedy: %v", base, err)
		}
	}
}

// A server on this machine, in the spellings people actually type.
func TestOrdinaryLocalhostAddressesAreAccepted(t *testing.T) {
	for _, base := range []string{
		"http://127.0.0.1:11434",
		"http://localhost:8080",
		"http://localhost:11434/v1",
		"http://[::1]:8080",
		"http://127.0.0.1:1234/v1",
	} {
		if err := CheckBaseURL(base); err != nil {
			t.Errorf("CheckBaseURL(%q) refused a local model server: %v", base, err)
		}
	}
}

// The hosted addresses people paste into the form.
func TestOrdinaryHostedAddressesAreAccepted(t *testing.T) {
	for _, base := range []string{
		"https://api.openai.com/v1",
		"https://openrouter.ai/api/v1",
		"https://api.groq.com/openai/v1",
		"https://my-resource.openai.azure.com",
	} {
		if err := CheckBaseURL(base); err != nil {
			t.Errorf("CheckBaseURL(%q) refused a hosted provider: %v", base, err)
		}
	}
}

// The same rule reaching a whole provider: a custom (OpenAI-wire) provider
// pointed at the LAN box does not construct, even with auth_style = "none",
// where there is no credential to protect. Pinned, not fixed — see above.
func TestOrdinaryCustomProviderOnTheLANIsRefused(t *testing.T) {
	if _, err := NewCustom("lan", config.ProviderConfig{
		Type: "custom", BaseURL: "http://192.168.1.50:8000/v1", AuthStyle: "none",
	}); err == nil {
		t.Error("a custom provider reached the LAN over plain http")
	}
	// https to the same box is the supported spelling and must work.
	if _, err := NewCustom("lan", config.ProviderConfig{
		Type: "custom", BaseURL: "https://192.168.1.50:8000/v1", AuthStyle: "none",
	}); err != nil {
		t.Errorf("a custom provider on the LAN over https was refused: %v", err)
	}
}

// Real AWS and GCP regions.
func TestOrdinaryCloudRegionsAreAccepted(t *testing.T) {
	for _, r := range []string{
		"us-east-1", "us-west-2", "eu-central-1", "ap-southeast-2", "eu-west-3",
		"us-gov-west-1", "cn-north-1",
		"us-east5", "europe-west4", "asia-northeast1", "global",
	} {
		if err := CheckRegion(r); err != nil {
			t.Errorf("CheckRegion(%q) refused a real region: %v", r, err)
		}
	}
}

// Bedrock and Vertex must build from an ordinary region and project.
func TestOrdinaryBedrockAndVertexConstruct(t *testing.T) {
	t.Setenv("AWS_ACCESS_KEY_ID", "AKIAEXAMPLE")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "secret")
	if _, err := NewBedrock("bedrock", config.ProviderConfig{
		Type: "bedrock", Region: "eu-central-1",
	}); err != nil && strings.Contains(err.Error(), "region") {
		t.Errorf("bedrock refused the region eu-central-1: %v", err)
	}
	if _, err := NewVertex("vertex", config.ProviderConfig{
		Type: "vertex", Region: "europe-west4", Project: "my-gcp-project-123",
		CredentialsFile: "/nonexistent.json",
	}); err != nil && strings.Contains(err.Error(), "region") {
		t.Errorf("vertex refused the region europe-west4: %v", err)
	}
}

// A GCP project id contains dashes and digits and may carry a domain prefix
// ("example.com:my-project" for legacy projects). It goes into the endpoint
// path, escaped, and must not be refused.
func TestOrdinaryVertexProjectIsAccepted(t *testing.T) {
	for _, p := range []string{"my-gcp-project-123", "acme-prod", "example.com:legacy-project"} {
		_, err := NewVertex("vertex", config.ProviderConfig{
			Type: "vertex", Region: "us-east5", Project: p,
			CredentialsFile: "/nonexistent.json",
		})
		if err != nil && strings.Contains(err.Error(), "project") &&
			!strings.Contains(err.Error(), "credentials") {
			t.Errorf("vertex refused the project %q: %v", p, err)
		}
	}
}

// The loopback test both packages share must recognise the spellings people use.
func TestOrdinaryLoopbackSpellings(t *testing.T) {
	for _, h := range []string{"localhost", "LOCALHOST", "localhost.", "127.0.0.1", "127.0.1.1", "::1", "[::1]"} {
		if !isLoopback(h) {
			t.Errorf("isLoopback(%q) says this machine is elsewhere", h)
		}
		if !config.IsLoopbackHost(h) {
			t.Errorf("config.IsLoopbackHost(%q) says this machine is elsewhere", h)
		}
	}
}
