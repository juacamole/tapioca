package ui

import (
	"context"
	"strings"
	"sync"
	"testing"

	"tapioca/internal/provider"
)

// stubProvider answers with fixed text so the ask path can be exercised
// without a network.
type stubProvider struct {
	name  string
	reply string

	// saw is written on the streaming goroutine and read by the test, so it
	// needs a lock. The race detector on CI caught this; it cannot run on an
	// arm64 kernel with a 47-bit VMA, which is where this was written.
	mu  sync.Mutex
	saw provider.Request
}

// request returns a copy of what the provider was asked for.
func (s *stubProvider) request() provider.Request {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.saw
}

func (s *stubProvider) Name() string { return s.name }

func (s *stubProvider) ListModels(context.Context) ([]string, error) {
	return []string{"stub-model"}, nil
}

func (s *stubProvider) Stream(ctx context.Context, req provider.Request, out chan<- provider.Event) (provider.Message, error) {
	s.mu.Lock()
	s.saw = req
	s.mu.Unlock()
	defer close(out)
	for _, word := range strings.Fields(s.reply) {
		select {
		case out <- provider.Event{Type: provider.EventTextDelta, Text: word + " "}:
		case <-ctx.Done():
			return provider.Message{}, ctx.Err()
		}
	}
	return provider.TextMessage("assistant", s.reply), nil
}

func askApp(t *testing.T, reply string) (*App, *stubProvider) {
	t.Helper()
	m := dashApp(t, 100, 30)
	st := &stubProvider{name: "stub", reply: reply}
	a := m.mgr.ActiveAgent()
	a.Provider, a.ProviderName, a.Model = st, "stub", "stub-model"
	a.Messages = []provider.Message{
		provider.TextMessage("user", "refactor the parser"),
		provider.TextMessage("assistant", "done"),
	}
	return m, st
}

// The whole point: the conversation is the same afterwards. If /ask appended
// anything, every later prompt would carry it and the transcript would show
// it — which is the cost this command exists to avoid.
func TestAskLeavesTheConversationUntouched(t *testing.T) {
	m, _ := askApp(t, "it strips absolute paths")
	a := m.mgr.ActiveAgent()

	before := make([]provider.Message, len(a.Messages))
	copy(before, a.Messages)

	cmd := cmdAsk(m, "what does --trimpath do?")
	if cmd == nil {
		t.Fatal("/ask produced no work")
	}
	// Drain the stream to completion.
	for i := 0; i < 50 && m.ask != nil && !m.ask.done; i++ {
		msg, ok := cmd().(askMsg)
		if !ok {
			break
		}
		cmd = m.handleAsk(msg)
		if cmd == nil {
			break
		}
	}

	if len(a.Messages) != len(before) {
		t.Fatalf("the history grew from %d to %d messages", len(before), len(a.Messages))
	}
	for i := range before {
		if a.Messages[i].Text() != before[i].Text() {
			t.Errorf("message %d changed to %q", i, a.Messages[i].Text())
		}
	}
}

// The question has to reach the model with the conversation behind it —
// answering without context would make it a worse search engine.
func TestAskSendsTheConversationAsContext(t *testing.T) {
	m, st := askApp(t, "yes")
	cmdAsk(m, "what did I just ask you to do?")

	// Let the goroutine run to the point it records the request.
	for i := 0; i < 50 && st.request().Model == ""; i++ {
		if m.ask == nil {
			break
		}
		if msg, ok := waitAsk(m.ask)().(askMsg); ok {
			m.handleAsk(msg)
			if msg.done {
				break
			}
		}
	}

	saw := st.request()
	if len(saw.Messages) < 3 {
		t.Fatalf("the model saw %d messages, want the conversation plus the question", len(saw.Messages))
	}
	last := saw.Messages[len(saw.Messages)-1].Text()
	if !strings.Contains(last, "what did I just ask you to do?") {
		t.Errorf("the question did not reach the model: %q", last)
	}
	if !strings.Contains(saw.Messages[0].Text(), "refactor the parser") {
		t.Error("the conversation was not sent as context")
	}
}

// An answer that could edit files is not a side question.
func TestAskOffersNoTools(t *testing.T) {
	m, st := askApp(t, "fine")
	cmdAsk(m, "anything")
	for i := 0; i < 50 && m.ask != nil && !m.ask.done; i++ {
		if msg, ok := waitAsk(m.ask)().(askMsg); ok {
			m.handleAsk(msg)
		}
	}
	if tools := st.request().Tools; len(tools) != 0 {
		t.Errorf("/ask offered %d tools", len(tools))
	}
}

// Escape closes the overlay and stops the request behind it.
func TestAskEscapeClosesAndCancels(t *testing.T) {
	m, _ := askApp(t, "a long answer that will not finish")
	cmdAsk(m, "something")
	if m.overlay != overlayAsk || m.ask == nil {
		t.Fatal("the ask overlay did not open")
	}
	m.closeAsk()
	if m.ask != nil || m.overlay != overlayNone {
		t.Error("escape did not close the overlay")
	}
}

// An empty question is a usage message, not a request.
func TestAskWithNoQuestionDoesNotCallTheModel(t *testing.T) {
	m, st := askApp(t, "unused")
	cmdAsk(m, "   ")
	if m.ask != nil {
		t.Error("an empty question opened the overlay")
	}
	if st.request().Model != "" {
		t.Error("an empty question reached the model")
	}
}

// The earlier test called cmdAsk directly and so missed this: runSlash notes
// every command in the transcript, which for /ask is exactly the trace it
// exists to avoid. Caught by running it in the real app, not by the tests.
func TestAskLeavesNoNoteInTheTranscript(t *testing.T) {
	m, _ := askApp(t, "an answer")
	a := m.mgr.ActiveAgent()
	before := len(a.Messages)

	m.runSlash("/ask what did I ask for?")

	if len(a.Messages) != before {
		t.Errorf("/ask added %d message(s) to the transcript", len(a.Messages)-before)
		for _, msg := range a.Messages[before:] {
			t.Logf("  %s: %q", msg.Role, msg.Text())
		}
	}
	m.closeAsk()
}

// Other commands must still be noted — the transcript is the record of what
// happened, and quietly dropping commands from it would be worse than the
// clutter /ask avoids.
func TestOtherCommandsAreStillNoted(t *testing.T) {
	m, _ := askApp(t, "unused")
	a := m.mgr.ActiveAgent()
	before := len(a.Messages)

	m.runSlash("/thinking")

	if len(a.Messages) == before {
		t.Error("/thinking left no note; the transcript no longer records commands")
	}
}
