package provider

import "strings"

// The Anthropic request shape changed across model generations, and the
// changes are removals rather than deprecations: sending the old shape to a
// current model is a 400, not a warning. So the payload is built from what the
// named model accepts instead of from one fixed shape.
//
// The trap that makes this worse than a version bump: omitting `thinking` does
// not mean "off" on the current models. Opus 5, Sonnet 5 and Fable 5 think by
// default, so a user who turns thinking off and gets the field omitted still
// gets thinking, and still pays for it. Off has to be said explicitly, in the
// spelling each model accepts.

// anthCaps is what one model's API accepts.
type anthCaps struct {
	adaptive    bool // thinking: {type: "adaptive"}
	budget      bool // thinking: {type: "enabled", budget_tokens: N}
	canDisable  bool // thinking: {type: "disabled"} is accepted
	thinksByDef bool // omitting thinking still thinks
	effort      bool // output_config: {effort: ...}
	sampling    bool // temperature / top_p / top_k are accepted
}

// anthCapsFor classifies a model id. Unknown ids get the current shape rather
// than the legacy one: a new model is far more likely to be newer than the
// ones listed here, and guessing old would send a removed parameter.
func anthCapsFor(model string) anthCaps {
	m := strings.ToLower(model)
	// Bedrock and Vertex prefix or suffix the same models.
	m = strings.TrimPrefix(m, "anthropic.")

	switch {
	// Thinking is always on and cannot be turned off.
	case contains(m, "fable", "mythos"):
		return anthCaps{adaptive: true, thinksByDef: true, effort: true}

	// Current generation: adaptive only, no sampling, thinking on by default.
	case contains(m, "opus-5", "opus5", "sonnet-5", "sonnet5"):
		return anthCaps{adaptive: true, canDisable: true, thinksByDef: true, effort: true}

	// 4.7 and 4.8 are adaptive-only too, but do not think unless asked.
	case contains(m, "opus-4-7", "opus-4-8"):
		return anthCaps{adaptive: true, canDisable: true, effort: true}

	// 4.6 accepts both shapes; adaptive is the recommended one, and sampling
	// still works here.
	case contains(m, "opus-4-6", "sonnet-4-6"):
		return anthCaps{adaptive: true, budget: true, canDisable: true, effort: true, sampling: true}

	// 4.5 and older: the budget is the API, and there is no adaptive mode.
	case contains(m, "-4-5", "-4-1", "claude-3", "opus-4-0", "sonnet-4-0",
		"claude-opus-4-2", "sonnet-4-20", "opus-4-20"):
		return anthCaps{budget: true, sampling: true}
	}
	return anthCaps{adaptive: true, canDisable: true, thinksByDef: true, effort: true}
}

func contains(s string, subs ...string) bool {
	for _, sub := range subs {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

// effortFor maps a thinking budget onto the effort levels current models use.
// Tapioca's own levels were defined as budgets, so this is the translation
// between the setting the user chose and the parameter the model takes.
func effortFor(budget int) string {
	switch {
	case budget <= 1024:
		return "low"
	case budget <= 4096:
		return "medium"
	default:
		return "high"
	}
}
