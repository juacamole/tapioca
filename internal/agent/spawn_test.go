package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"tapioca/internal/config"
	"tapioca/internal/provider"
)

// drain consumes an agent's events and returns the first spawn request.
func firstSpawn(t *testing.T, a *Agent) *SpawnReq {
	t.Helper()
	select {
	case ev := <-a.Events:
		if ev.Kind != EvSpawn || ev.Spawn == nil {
			t.Fatalf("expected a spawn event, got kind %v", ev.Kind)
		}
		return ev.Spawn
	case <-time.After(2 * time.Second):
		t.Fatal("no spawn event emitted")
		return nil
	}
}

func TestSpawnBlocksUntilAnswered(t *testing.T) {
	a := &Agent{Events: make(chan Event, 8)}
	type outcome struct {
		text  string
		isErr bool
	}
	done := make(chan outcome, 1)
	go func() {
		text, isErr := a.spawnAgent(context.Background(),
			json.RawMessage(`{"task":"count the go files","name":"counter"}`))
		done <- outcome{text, isErr}
	}()

	req := firstSpawn(t, a)
	if req.Task != "count the go files" || req.Name != "counter" {
		t.Fatalf("task/name not passed through: %+v", req)
	}
	select {
	case o := <-done:
		t.Fatalf("spawn returned before an answer arrived: %+v", o)
	case <-time.After(50 * time.Millisecond):
	}

	req.Reply <- SpawnResult{Text: "there are 12"}
	o := <-done
	if o.isErr || o.text != "there are 12" {
		t.Fatalf("answer not returned as the tool result: %+v", o)
	}
}

func TestSpawnReportsFailureAndEmptyAnswer(t *testing.T) {
	for _, tc := range []struct {
		name   string
		result SpawnResult
		want   string
	}{
		{"error", SpawnResult{Err: context.DeadlineExceeded}, "the subagent failed"},
		{"empty", SpawnResult{Text: "   "}, "without producing an answer"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := &Agent{Events: make(chan Event, 8)}
			done := make(chan string, 1)
			go func() {
				text, isErr := a.spawnAgent(context.Background(), json.RawMessage(`{"task":"x"}`))
				if !isErr {
					t.Errorf("expected an error result, got %q", text)
				}
				done <- text
			}()
			firstSpawn(t, a).Reply <- tc.result
			if got := <-done; !strings.Contains(got, tc.want) {
				t.Errorf("got %q, want it to mention %q", got, tc.want)
			}
		})
	}
}

func TestSpawnCancelledWithTheParent(t *testing.T) {
	a := &Agent{Events: make(chan Event, 8)}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan string, 1)
	go func() {
		text, _ := a.spawnAgent(ctx, json.RawMessage(`{"task":"long one"}`))
		done <- text
	}()
	firstSpawn(t, a)
	cancel()
	select {
	case got := <-done:
		if !strings.Contains(got, "cancelled") {
			t.Errorf("got %q, want a cancellation notice", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("cancelling the parent did not release the spawn")
	}
}

func TestSubagentsCannotSpawn(t *testing.T) {
	a := &Agent{Events: make(chan Event, 8), Depth: 1}
	text, isErr := a.spawnAgent(context.Background(), json.RawMessage(`{"task":"recurse"}`))
	if !isErr || !strings.Contains(text, "cannot spawn") {
		t.Fatalf("a subagent was allowed to spawn: %q", text)
	}
	select {
	case ev := <-a.Events:
		t.Fatalf("refusal still emitted an event: %v", ev.Kind)
	default:
	}
}

func TestSpawnRejectsEmptyTask(t *testing.T) {
	a := &Agent{Events: make(chan Event, 8)}
	for _, raw := range []string{`{"task":"  "}`, `{}`, `not json`} {
		text, isErr := a.spawnAgent(context.Background(), json.RawMessage(raw))
		if !isErr || !strings.Contains(text, "invalid arguments") {
			t.Errorf("%s accepted: %q", raw, text)
		}
	}
}

func TestSpawnedAgentHasOwnContextAndDepth(t *testing.T) {
	mgr := NewManager(&config.Config{}, nil, nil)
	parent := &Agent{
		ID: 1, Name: "agent-1", Model: "m", SystemPrompt: "sys",
		ToolsEnabled: true,
	}
	parent.Messages = append(parent.Messages, provider.TextMessage("user", "hi"))
	mgr.Agents = []*Agent{parent}

	sub := mgr.Spawn(parent, "")
	if sub.Depth != 1 {
		t.Errorf("subagent depth = %d, want 1", sub.Depth)
	}
	if len(sub.Messages) != 0 {
		t.Errorf("subagent inherited history: %+v", sub.Messages)
	}
	if sub.Model != parent.Model || sub.SystemPrompt != parent.SystemPrompt {
		t.Error("subagent did not inherit the parent's model/prompt")
	}
	if !strings.HasPrefix(sub.Name, "task-") {
		t.Errorf("unnamed subagent got %q, want a task- label", sub.Name)
	}
	if len(mgr.Agents) != 2 {
		t.Error("subagent was not registered as a tab")
	}
}
