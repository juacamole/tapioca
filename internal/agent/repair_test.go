package agent

import (
	"encoding/json"
	"testing"

	"tapioca/internal/provider"
)

func toolUse(id string, input string) provider.Message {
	return provider.Message{Role: "assistant", Blocks: []provider.Block{
		{Type: "text", Text: "calling"},
		{Type: "tool_use", ID: id, Name: "bash", Input: json.RawMessage(input)},
	}}
}

func toolResult(id string) provider.Message {
	return provider.Message{Role: "user", Blocks: []provider.Block{
		{Type: "tool_result", ToolUseID: id, Name: "bash", Content: "ok"},
	}}
}

func user(text string) provider.Message { return provider.TextMessage("user", text) }

func TestRepairAddsMissingResult(t *testing.T) {
	out := RepairHistory([]provider.Message{user("hi"), toolUse("t1", `{}`)})
	if len(out) != 3 {
		t.Fatalf("want 3 messages, got %d", len(out))
	}
	last := out[2]
	if !last.IsToolResult() || last.Blocks[0].ToolUseID != "t1" || !last.Blocks[0].IsError {
		t.Fatalf("missing synthetic tool_result: %+v", last)
	}
}

func TestRepairInterleavedUserMessage(t *testing.T) {
	// A /btw-style message wedged between use and result: the stray result
	// is dropped and a synthetic one closes the pair.
	out := RepairHistory([]provider.Message{
		toolUse("t1", `{}`), user("side note"), toolResult("t1"),
	})
	for i, m := range out {
		if m.IsToolResult() && i > 0 && len(out[i-1].ToolUses()) == 0 {
			t.Fatalf("tool_result at %d does not follow tool_use", i)
		}
	}
	if len(out[0].ToolUses()) == 1 && !out[1].IsToolResult() {
		t.Fatalf("pair severed: %+v", out[1])
	}
}

func TestRepairDropsStrayAndDuplicate(t *testing.T) {
	out := RepairHistory([]provider.Message{
		toolResult("ghost"),
		toolUse("t1", `{}`), toolResult("t1"), toolResult("t1"),
	})
	seen := 0
	for _, m := range out {
		if m.IsToolResult() {
			seen++
			if m.Blocks[0].ToolUseID != "t1" {
				t.Fatalf("stray result kept: %+v", m)
			}
		}
	}
	if seen != 1 {
		t.Fatalf("want exactly 1 result, got %d", seen)
	}
}

func TestRepairFixesInvalidInput(t *testing.T) {
	out := RepairHistory([]provider.Message{toolUse("t1", `{"cmd": "trunc`)})
	if got := string(out[0].ToolUses()[0].Input); got != "{}" {
		t.Fatalf("invalid input not repaired: %q", got)
	}
}

func TestRepairKeepsCleanHistory(t *testing.T) {
	in := []provider.Message{user("hi"), toolUse("t1", `{"a":1}`), toolResult("t1"), user("thanks")}
	out := RepairHistory(in)
	if len(out) != len(in) {
		t.Fatalf("clean history changed: %d -> %d", len(in), len(out))
	}
}
