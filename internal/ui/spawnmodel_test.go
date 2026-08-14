package ui

import (
	"strings"
	"testing"

	"tapioca/internal/agent"
	"tapioca/internal/config"
	"tapioca/internal/provider"
)

// A subagent always ran on the parent's model, so delegating a wide file
// search cost the same as reasoning with it. The point of the option is to
// send cheap work somewhere cheap.
func TestSubagentRunsOnTheRequestedModel(t *testing.T) {
	m := dashApp(t, 100, 30)
	m.cfg.Providers["ollama"] = config.ProviderConfig{Type: "ollama", BaseURL: "http://localhost:11434"}

	parent := m.mgr.ActiveAgent()
	parent.ProviderName, parent.Model = "anthropic", "claude-opus-5"
	child := &agent.Agent{}

	if err := m.applySpawnModel(child, parent, "ollama:qwen3-coder"); err != nil {
		t.Fatalf("applySpawnModel: %v", err)
	}
	if child.ProviderName != "ollama" || child.Model != "qwen3-coder" {
		t.Errorf("child is on %s:%s, want ollama:qwen3-coder", child.ProviderName, child.Model)
	}
	if parent.ProviderName != "anthropic" || parent.Model != "claude-opus-5" {
		t.Errorf("the parent was moved to %s:%s", parent.ProviderName, parent.Model)
	}
}

// With no provider prefix the parent's provider is used, so "give it a cheaper
// model on the same account" does not need the provider spelled out.
func TestSubagentModelWithoutAProviderKeepsTheParentsProvider(t *testing.T) {
	m := dashApp(t, 100, 30)
	parent := m.mgr.ActiveAgent()
	// A running parent always holds a built provider; that instance is what a
	// same-provider child should inherit.
	prov, err := provider.New("anthropic", config.ProviderConfig{Type: "anthropic", APIKey: "k"})
	if err != nil {
		t.Fatal(err)
	}
	parent.Provider, parent.ProviderName, parent.Model = prov, "anthropic", "claude-opus-5"
	child := &agent.Agent{}

	if err := m.applySpawnModel(child, parent, "claude-haiku-4-5"); err != nil {
		t.Fatal(err)
	}
	if child.Provider != prov {
		t.Error("the child rebuilt the provider instead of reusing the parent's")
	}
	if child.ProviderName != "anthropic" || child.Model != "claude-haiku-4-5" {
		t.Errorf("child is on %s:%s, want anthropic:claude-haiku-4-5", child.ProviderName, child.Model)
	}
}

// A gateway model id contains a slash, not a colon, and a colon that names no
// configured provider is part of the model name. Reading either as a provider
// would send the task somewhere it was never asked to go.
func TestModelRefIsNotConfusedByGatewayIDs(t *testing.T) {
	m := dashApp(t, 100, 30)
	m.cfg.Providers["vercel"] = config.ProviderConfig{Type: "vercel"}

	for _, tc := range []struct{ ref, wantProv, wantModel string }{
		{"anthropic/claude-opus-5", "vercel", "anthropic/claude-opus-5"}, // gateway id, no prefix
		{"vercel:anthropic/claude-opus-5", "vercel", "anthropic/claude-opus-5"},
		{"notaprovider:something", "vercel", "notaprovider:something"}, // unknown prefix stays in the name
	} {
		gotProv, gotModel := m.splitModelRef(tc.ref, "vercel")
		if gotProv != tc.wantProv || gotModel != tc.wantModel {
			t.Errorf("splitModelRef(%q) = %s:%s, want %s:%s", tc.ref, gotProv, gotModel, tc.wantProv, tc.wantModel)
		}
	}
}

// An unresolvable provider has to fail the tool call. Running the task on the
// parent's model while the tab claims another is the outcome worth avoiding.
func TestUnknownProviderIsRefusedAndNamesTheOptions(t *testing.T) {
	m := dashApp(t, 100, 30)
	parent := m.mgr.ActiveAgent()
	parent.ProviderName = "anthropic"
	child := &agent.Agent{}

	err := m.applySpawnModel(child, parent, "nope:some-model")
	if err == nil {
		t.Fatal("an unknown provider was accepted")
	}
	// "nope" is not configured, so it stays part of the model name and the
	// parent's provider is used — which must itself resolve or fail loudly.
	if child.Provider != nil {
		t.Error("the child was left pointing at a provider despite the failure")
	}
	if !strings.Contains(err.Error(), "configured providers") {
		t.Errorf("the error does not say what could have been asked for: %v", err)
	}
}

// The tool has to advertise the option, or no model will ever use it.
func TestSpawnSchemaOffersAModel(t *testing.T) {
	schema := string(agent.SpawnTool.InputSchema)
	if !strings.Contains(schema, `"model"`) {
		t.Error("spawn_agent does not accept a model")
	}
	if !strings.Contains(agent.SpawnTool.Description, "model") {
		t.Error("the tool description never mentions choosing a model")
	}
	// task stays the only required field, so existing behaviour is unchanged.
	if !strings.Contains(schema, `"required":["task"]`) {
		t.Errorf("required fields changed: %s", schema)
	}
}
