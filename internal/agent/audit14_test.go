package agent

import (
	"context"
	"testing"
	"time"

	"tapioca/internal/provider"
)

// hostileUsageProvider is a model server that lies about tokens. Everything
// else in a streamed response is parsed and bounded; the counts were assigned
// through untouched, and they are the ones the UI divides and multiplies rather
// than printing.
type hostileUsageProvider struct{ usage provider.Usage }

func (h *hostileUsageProvider) Name() string                                 { return "hostile" }
func (h *hostileUsageProvider) ListModels(context.Context) ([]string, error) { return nil, nil }
func (h *hostileUsageProvider) Stream(ctx context.Context, req provider.Request, out chan<- provider.Event) (provider.Message, error) {
	defer close(out)
	out <- provider.Event{Type: provider.EventDone, StopReason: "end_turn"}
	msg := provider.TextMessage("assistant", "ok")
	u := h.usage
	msg.Usage = &u
	return msg, nil
}

func usageFromTurn(t *testing.T, u provider.Usage) provider.Usage {
	t.Helper()
	a := &Agent{
		ID: 1, Provider: &hostileUsageProvider{usage: u}, ProviderName: "hostile",
		Model: "m", Events: make(chan Event, 512),
	}
	a.Send([]provider.Message{provider.TextMessage("user", "hi")})
	deadline := time.After(5 * time.Second)
	var got *provider.Usage
	for {
		select {
		case ev := <-a.Events:
			if ev.Kind == EvUsage && ev.Usage != nil {
				got = ev.Usage
			}
			if ev.Kind == EvDone {
				if got == nil {
					t.Fatal("control: the turn reported no usage at all, so nothing crossed the boundary")
				}
				return *got
			}
		case <-deadline:
			t.Fatal("agent never finished")
		}
	}
}

// The counts a server sends become a request's Out in Stats and the agent's
// CtxTokens, and from there the dashboard scales them. A negative one indexes a
// rune slice of eight at a negative offset; a large positive one overflows
// CtxTokens*100 into a negative percent that strings.Repeat panics on. Both
// pass every guard on the way: the assignment is gated on input *or* output
// being positive, so the other field can be anything.
func TestATurnPublishesOnlyBelievableTokenCounts(t *testing.T) {
	for _, tc := range []struct {
		name string
		u    provider.Usage
	}{
		{"a negative output count", provider.Usage{InputTokens: 1, OutputTokens: -5}},
		{"a count that overflows the gauge", provider.Usage{InputTokens: 100000000000000000, OutputTokens: 1}},
		{"every field at once", provider.Usage{
			InputTokens: -1, OutputTokens: 1 << 62, CacheReadTokens: -1 << 62, CacheWriteTokens: 1 << 62}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := usageFromTurn(t, tc.u)
			for name, v := range map[string]int{
				"input": got.InputTokens, "output": got.OutputTokens,
				"cache read": got.CacheReadTokens, "cache write": got.CacheWriteTokens,
			} {
				if v < 0 {
					t.Errorf("%s tokens reached the UI negative: %d", name, v)
				}
			}
			// The sum becomes CtxTokens and the context gauge multiplies it by
			// a hundred before dividing. Anything that leaves room for that
			// cannot wrap into a negative percent.
			sum := got.InputTokens + got.OutputTokens + got.CacheReadTokens + got.CacheWriteTokens
			if sum < 0 || sum*100 < 0 {
				t.Errorf("the published total still overflows when the gauge scales it: %d", sum)
			}
		})
	}
}

// The ordinary half, with equal weight: honest counts must arrive untouched or
// the cost line and the context gauge stop meaning anything.
func TestOrdinaryTokenCountsReachTheUIUnchanged(t *testing.T) {
	want := provider.Usage{InputTokens: 190_432, OutputTokens: 2_311,
		CacheReadTokens: 180_000, CacheWriteTokens: 4_096}
	if got := usageFromTurn(t, want); got != want {
		t.Errorf("an ordinary usage was altered: %+v -> %+v", want, got)
	}
}

// retryableProvider is a model server that only ever fails, in the class the
// run loop retries: a rate limit, a 503, a provider having trouble. One
// endpoint can back both entries of a fallback chain, so one hostile — or
// merely unwell — server reaches every attempt below.
//
// The first one asks for a wait longer than Tapioca will sit through, which is
// the other door to the fallback and the one that does not take a minute of
// sleeping to walk through.
type retryableProvider struct {
	name  string
	after time.Duration
}

func (p *retryableProvider) Name() string                                 { return p.name }
func (p *retryableProvider) ListModels(context.Context) ([]string, error) { return nil, nil }
func (p *retryableProvider) Stream(ctx context.Context, req provider.Request, out chan<- provider.Event) (provider.Message, error) {
	close(out)
	return provider.Message{}, &provider.APIError{
		Provider: p.name, Status: 503, Message: "overloaded", RetryAfter: p.after,
	}
}

// The retry loop counts from 1 and RetryDelay shifts by attempt-1. On a
// fallback the loop resets the counter to -1 so that its own ++ makes the next
// pass "the first attempt there" — but the loop's first attempt is 1, and
// -1 + 1 is 0. The first retryable failure against the fallback provider then
// calls RetryDelay(0), which shifts by -1, and a negative shift is a runtime
// panic rather than an error: the process dies mid-turn, with the terminal
// still in raw mode and the session unsaved.
//
// Nothing short-circuits it — RetryDelay is called on the line before the
// attempt >= RetryMaxAttempts test, not after it.
//
// It needs no hostility at all to fire. A primary and a fallback that are both
// briefly overloaded is the exact situation a fallback chain exists for.
func TestAFailingFallbackDoesNotBringTheProcessDown(t *testing.T) {
	first := &retryableProvider{name: "first", after: 2 * time.Hour}
	second := &retryableProvider{name: "second", after: 2 * time.Hour}
	a := &Agent{
		ID: 1, Provider: first, ProviderName: "first", Model: "m",
		Events:    make(chan Event, 512),
		Fallbacks: []fallbackTarget{{prov: second, providerName: "second", model: "m2"}},
	}
	a.Send([]provider.Message{provider.TextMessage("user", "hi")})

	sawFallback := false
	deadline := time.After(30 * time.Second)
	for {
		select {
		case ev := <-a.Events:
			switch ev.Kind {
			case EvFallback:
				sawFallback = true
			case EvDone:
				if !sawFallback {
					t.Fatal("control: the turn never reached the fallback, so it never reached the reset counter")
				}
				return // it got to the end without taking the process with it
			}
		case <-deadline:
			t.Fatal("the turn never finished")
		}
	}
}
