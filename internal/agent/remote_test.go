package agent

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"tapioca/internal/config"
	"tapioca/internal/provider"
)

// fakeRemote stands in for an external agent, so the routing can be tested
// without a process on the other end.
type fakeRemote struct {
	mu     sync.Mutex
	prompt string
	closed bool
	err    error
	block  chan struct{} // when set, Prompt waits on it or on cancellation
}

func (f *fakeRemote) Prompt(ctx context.Context, text string) error {
	f.mu.Lock()
	f.prompt = text
	block, err := f.block, f.err
	f.mu.Unlock()
	if block != nil {
		select {
		case <-block:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return err
}

func (f *fakeRemote) Close() {
	f.mu.Lock()
	f.closed = true
	f.mu.Unlock()
}

func (f *fakeRemote) sent() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.prompt
}

func (f *fakeRemote) wasClosed() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.closed
}

func collect(t *testing.T, a *Agent) []Event {
	t.Helper()
	var evs []Event
	for {
		select {
		case ev := <-a.Events:
			evs = append(evs, ev)
			if ev.Kind == EvDone {
				return evs
			}
		case <-time.After(5 * time.Second):
			t.Fatal("the turn never ended")
		}
	}
}

func TestSendGoesToTheRemote(t *testing.T) {
	f := &fakeRemote{}
	mgr := NewManager(config.Default(), nil, nil)
	a := mgr.NewExternal("ext", f)

	a.Send([]provider.Message{
		provider.TextMessage("user", "first"),
		provider.TextMessage("assistant", "ok"),
		provider.TextMessage("user", "second"),
	})
	collect(t, a)
	// The agent on the other end keeps the conversation, so only the new
	// prompt crosses.
	if got := f.sent(); got != "second" {
		t.Errorf("sent %q, want the newest user message", got)
	}
}

func TestRemoteFailureStaysOnItsTab(t *testing.T) {
	f := &fakeRemote{err: errors.New("it died")}
	mgr := NewManager(config.Default(), nil, nil)
	a := mgr.NewExternal("ext", f)
	other := mgr.NewAgent()

	a.Send([]provider.Message{provider.TextMessage("user", "go")})
	evs := collect(t, a)

	var reported bool
	for _, ev := range evs {
		if ev.Kind == EvError && ev.Err != nil && ev.Err.Error() == "it died" {
			reported = true
		}
	}
	if !reported {
		t.Error("the failure was never reported")
	}
	if evs[len(evs)-1].Kind != EvDone {
		t.Error("a failed turn must still end, or the tab stays busy forever")
	}
	if len(other.Events) != 0 || other.Status != StatusIdle {
		t.Error("the failure reached another agent")
	}
}

func TestCancelReachesTheRemote(t *testing.T) {
	f := &fakeRemote{block: make(chan struct{})}
	mgr := NewManager(config.Default(), nil, nil)
	a := mgr.NewExternal("ext", f)

	a.Send([]provider.Message{provider.TextMessage("user", "take your time")})
	a.Cancel()
	collect(t, a)
}

func TestClosingTheTabDisconnects(t *testing.T) {
	f := &fakeRemote{}
	mgr := NewManager(config.Default(), nil, nil)
	mgr.NewAgent()
	mgr.NewExternal("ext", f)

	mgr.Close(1)
	if !f.wasClosed() {
		t.Error("closing the tab left the external process running")
	}
}

// A connection is not state a file can hold: a saved session that claimed to
// have one would come back as a tab that answers nothing.
func TestExternalAgentsAreNotSaved(t *testing.T) {
	f := &fakeRemote{}
	mgr := NewManager(config.Default(), nil, nil)
	local := mgr.NewAgent()
	mgr.NewExternal("ext", f)
	mgr.Active = 1

	s := mgr.ToSession("id", "name", time.Now())
	if len(s.Agents) != 1 || s.Agents[0].Name != local.Name {
		t.Fatalf("saved agents = %+v, want only the local one", s.Agents)
	}
	if s.Active != 0 {
		t.Errorf("active = %d, want an index into what was actually saved", s.Active)
	}
}
