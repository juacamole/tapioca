package agent

import (
	"encoding/json"
	"time"

	"tapioca/internal/provider"
)

// RepairHistory enforces the provider protocol invariants that every history
// mutation site can otherwise violate: every tool_use gets a matching
// tool_result before the next real message, stray or duplicate tool_results
// are dropped, and tool_use inputs are valid JSON. One malformed entry would
// 400 every subsequent request until /clear.
func RepairHistory(in []provider.Message) []provider.Message {
	out := make([]provider.Message, 0, len(in))
	for i := 0; i < len(in); i++ {
		m := in[i]
		if m.IsToolResult() {
			continue // stray result with no preceding tool_use
		}
		uses := m.ToolUses()
		if m.Role == "assistant" && len(uses) > 0 {
			m = sanitizeInputs(m)
			out = append(out, m)
			pending := map[string]bool{}
			for _, u := range m.ToolUses() {
				pending[u.ID] = true
			}
			j := i + 1
			for ; j < len(in) && in[j].IsToolResult(); j++ {
				kept := in[j]
				var blocks []provider.Block
				for _, b := range kept.Blocks {
					if pending[b.ToolUseID] {
						pending[b.ToolUseID] = false
						blocks = append(blocks, b)
					}
				}
				if len(blocks) > 0 {
					kept.Blocks = blocks
					out = append(out, kept)
				}
			}
			i = j - 1
			var missing []provider.Block
			for _, u := range m.ToolUses() {
				if pending[u.ID] {
					missing = append(missing, interruptedResult(u))
				}
			}
			if len(missing) > 0 {
				out = append(out, provider.Message{Role: "user", Blocks: missing, Time: time.Now()})
			}
			continue
		}
		out = append(out, m)
	}
	return out
}

func sanitizeInputs(m provider.Message) provider.Message {
	fixed := m
	fixed.Blocks = make([]provider.Block, len(m.Blocks))
	copy(fixed.Blocks, m.Blocks)
	for i, b := range fixed.Blocks {
		if b.Type == "tool_use" && len(b.Input) > 0 && !json.Valid(b.Input) {
			fixed.Blocks[i].Input = json.RawMessage("{}")
		}
	}
	return fixed
}
