package provider

import (
	"encoding/json"
	"strings"
	"testing"
)

// These assert the JSON, not the Go struct. The bug being fixed was entirely
// about what reached the wire — the struct was fine and the request was still
// rejected.
func payloadFor(t *testing.T, model string, thinking bool, budget int, temp float64) map[string]any {
	t.Helper()
	body := anthReq{Model: model, MaxTokens: 4096}
	applyThinking(&body, Request{Model: model, Thinking: thinking, ThinkingBudget: budget, Temperature: temp})
	data, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// budget_tokens is removed, not deprecated, on current models: sending it is a
// 400. This is the reported bug.
func TestCurrentModelsGetAdaptiveNotABudget(t *testing.T) {
	for _, model := range []string{
		"claude-opus-5", "claude-sonnet-5", "claude-opus-4-8", "claude-opus-4-7",
		"claude-fable-5", "anthropic.claude-opus-5",
	} {
		p := payloadFor(t, model, true, 4096, 1)
		th, ok := p["thinking"].(map[string]any)
		if !ok {
			t.Errorf("%s: no thinking object sent", model)
			continue
		}
		if th["type"] != "adaptive" {
			t.Errorf("%s: thinking.type = %v, want adaptive", model, th["type"])
		}
		if _, present := th["budget_tokens"]; present {
			t.Errorf("%s: budget_tokens was sent, which the model rejects", model)
		}
		if _, present := p["temperature"]; present {
			t.Errorf("%s: temperature was sent, which the model rejects", model)
		}
	}
}

// The effort setting has to reach the request, or /effort controls nothing.
func TestEffortReachesTheRequest(t *testing.T) {
	cases := map[int]string{512: "low", 4096: "medium", 16384: "high"}
	for budget, want := range cases {
		p := payloadFor(t, "claude-opus-5", true, budget, 1)
		oc, ok := p["output_config"].(map[string]any)
		if !ok {
			t.Errorf("budget %d: no output_config sent", budget)
			continue
		}
		if oc["effort"] != want {
			t.Errorf("budget %d: effort = %v, want %s", budget, oc["effort"], want)
		}
	}
}

// Turning thinking off was the failing case: the old code took the else branch
// and sent temperature, which current models reject.
func TestThinkingOffSendsNoSamplingOnCurrentModels(t *testing.T) {
	for _, model := range []string{"claude-opus-5", "claude-sonnet-5", "claude-opus-4-8"} {
		p := payloadFor(t, model, false, 0, 0.7)
		if _, present := p["temperature"]; present {
			t.Errorf("%s: temperature sent with thinking off — this is the reported 400", model)
		}
	}
}

// Omitting the field is not "off" on models that think by default: the user
// would turn thinking off, still get thinking, and still pay for it.
func TestOffIsSaidExplicitlyWhereOmittingWouldStillThink(t *testing.T) {
	for _, model := range []string{"claude-opus-5", "claude-sonnet-5", "claude-opus-4-8"} {
		p := payloadFor(t, model, false, 0, -1)
		th, ok := p["thinking"].(map[string]any)
		if !ok {
			t.Errorf("%s: thinking omitted with thinking off — the model thinks anyway", model)
			continue
		}
		if th["type"] != "disabled" {
			t.Errorf("%s: thinking.type = %v, want disabled", model, th["type"])
		}
	}
}

// Fable and Mythos have no off: an explicit disabled is a 400, and omitting is
// the only way to express it.
func TestFableIsNeverSentAnExplicitDisable(t *testing.T) {
	for _, model := range []string{"claude-fable-5", "claude-mythos-5"} {
		p := payloadFor(t, model, false, 0, -1)
		if th, present := p["thinking"]; present {
			t.Errorf("%s: sent thinking %v, which is rejected — it must be omitted", model, th)
		}
	}
}

// The old shape is still the API on pre-4.6 models, so this cannot become a
// blanket swap.
func TestOlderModelsKeepTheBudget(t *testing.T) {
	for _, model := range []string{"claude-sonnet-4-5", "claude-haiku-4-5", "claude-3-opus-20240229"} {
		p := payloadFor(t, model, true, 8192, 1)
		th, ok := p["thinking"].(map[string]any)
		if !ok {
			t.Fatalf("%s: no thinking object sent", model)
		}
		if th["type"] != "enabled" {
			t.Errorf("%s: thinking.type = %v, want enabled", model, th["type"])
		}
		if th["budget_tokens"] != float64(8192) {
			t.Errorf("%s: budget_tokens = %v, want 8192", model, th["budget_tokens"])
		}
		if _, present := p["output_config"]; present {
			t.Errorf("%s: effort sent to a model that does not take it", model)
		}
	}
}

// A budget at or above max_tokens leaves no room for an answer.
func TestBudgetLeavesRoomForTheAnswer(t *testing.T) {
	body := anthReq{Model: "claude-sonnet-4-5", MaxTokens: 4096}
	applyThinking(&body, Request{Model: "claude-sonnet-4-5", Thinking: true, ThinkingBudget: 8192})
	if body.MaxTokens <= 8192 {
		t.Errorf("max_tokens = %d, not above the %d budget", body.MaxTokens, 8192)
	}
}

// An unknown id is far more likely to be newer than the ones listed, and
// guessing old would send a removed parameter to it.
func TestUnknownModelsGetTheCurrentShape(t *testing.T) {
	p := payloadFor(t, "claude-something-not-released-yet", true, 4096, 1)
	th, _ := p["thinking"].(map[string]any)
	if th["type"] != "adaptive" {
		t.Errorf("unknown model got %v, want adaptive", th["type"])
	}
	if _, present := th["budget_tokens"]; present {
		t.Error("unknown model was sent budget_tokens")
	}
}

// Sampling is still accepted on 4.6, so it must not be stripped there.
func TestSamplingSurvivesWhereItIsStillAccepted(t *testing.T) {
	p := payloadFor(t, "claude-sonnet-4-6", false, 0, 0.7)
	if _, present := p["temperature"]; !present {
		t.Error("temperature was dropped on a model that accepts it")
	}
}

// The capability table is keyed on substrings, so a provider prefix must not
// change the answer.
func TestProviderPrefixesDoNotChangeCapabilities(t *testing.T) {
	plain := anthCapsFor("claude-opus-5")
	for _, variant := range []string{"anthropic.claude-opus-5", "CLAUDE-OPUS-5"} {
		if got := anthCapsFor(variant); got != plain {
			t.Errorf("%s classified differently from the bare id", variant)
		}
	}
	if !strings.Contains("anthropic.claude-opus-5", "opus-5") {
		t.Skip()
	}
}
