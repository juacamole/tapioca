package agent

import (
	"context"
	"encoding/json"
	"strings"

	"tapioca/internal/provider"
)

// SpawnReq asks the UI to run a task in a fresh agent. Exactly one result
// must be sent on Reply, or the delegating agent waits forever.
type SpawnReq struct {
	Task  string
	Name  string
	Reply chan SpawnResult
}

// SpawnResult is a finished subagent's answer.
type SpawnResult struct {
	Text string
	Err  error
}

// SpawnTool delegates a self-contained task to an agent with its own context
// window. Only top-level agents get it: a subagent that could spawn subagents
// turns one prompt into unbounded fan-out.
var SpawnTool = provider.ToolDef{
	Name: "spawn_agent",
	Description: "Delegate a self-contained task to a background agent with its own context window, " +
		"and get back only its final answer. Use it for work whose intermediate output you do not need " +
		"— wide searches, surveying many files, independent investigations. The subagent starts with no " +
		"knowledge of this conversation, so put everything it needs in the task, and tell it what to report back.",
	InputSchema: json.RawMessage(`{"type":"object","properties":{"task":{"type":"string","description":"self-contained instructions, including what to report back"},"name":{"type":"string","description":"short label for the agent tab"}},"required":["task"]}`),
}

// spawnAgent blocks until the subagent finishes and returns its answer as the
// tool result. The parent's context cancels the wait, so ctrl+c still works.
func (a *Agent) spawnAgent(ctx context.Context, raw json.RawMessage) (string, bool) {
	var in struct {
		Task string `json:"task"`
		Name string `json:"name"`
	}
	if err := json.Unmarshal(raw, &in); err != nil || strings.TrimSpace(in.Task) == "" {
		return "invalid arguments: need {\"task\": \"...\"}", true
	}
	if a.Depth > 0 {
		return "refused: subagents cannot spawn further agents — do this task yourself", true
	}

	req := &SpawnReq{
		Task:  strings.TrimSpace(in.Task),
		Name:  strings.TrimSpace(in.Name),
		Reply: make(chan SpawnResult, 1),
	}
	a.emit(Event{Kind: EvSpawn, Spawn: req})
	select {
	case res := <-req.Reply:
		if res.Err != nil {
			return "the subagent failed: " + res.Err.Error(), true
		}
		if strings.TrimSpace(res.Text) == "" {
			return "the subagent finished without producing an answer", true
		}
		return res.Text, false
	case <-ctx.Done():
		return "the delegated task was cancelled", true
	}
}
