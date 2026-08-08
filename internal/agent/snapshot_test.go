package agent

import (
	"context"
	"testing"
	"time"

	"tapioca/internal/provider"
)

type captureProvider struct{ req chan provider.Request }

func (c *captureProvider) Name() string                                 { return "cap" }
func (c *captureProvider) ListModels(context.Context) ([]string, error) { return nil, nil }
func (c *captureProvider) Stream(ctx context.Context, req provider.Request, out chan<- provider.Event) (provider.Message, error) {
	defer close(out)
	c.req <- req
	out <- provider.Event{Type: provider.EventDone, StopReason: "end_turn"}
	return provider.TextMessage("assistant", "ok"), nil
}

func drain(t *testing.T, a *Agent) {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case ev := <-a.Events:
			if ev.Kind == EvDone {
				return
			}
		case <-deadline:
			t.Fatal("agent never finished")
		}
	}
}

// Mid-turn settings changes must not affect the running turn: the run
// goroutine works from a snapshot taken at Send.
func TestSendSnapshotsSettings(t *testing.T) {
	cp := &captureProvider{req: make(chan provider.Request, 4)}
	a := &Agent{
		ID: 1, Provider: cp, ProviderName: "cap", Model: "m1",
		SystemPrompt: "base", MaxTokens: 42,
		Events: make(chan Event, 512),
	}
	a.Send([]provider.Message{provider.TextMessage("user", "hi")})
	a.Model = "m2"
	a.Provider = nil
	a.MaxTokens = 9

	got := <-cp.req
	if got.Model != "m1" || got.MaxTokens != 42 {
		t.Fatalf("turn used mutated settings: model=%q maxTokens=%d", got.Model, got.MaxTokens)
	}
	drain(t, a)
}
