package ui

import (
	"context"
	"strings"

	tea "charm.land/bubbletea/v2"

	"tapioca/internal/agent"
	"tapioca/internal/provider"
)

// Asking the model a passing question used to cost the conversation: anything
// typed became history, was resent with every later prompt, and stayed in the
// transcript. So "what does this flag do?" permanently changed a session you
// were part way through.
//
// /ask answers from the same context and leaves nothing behind. It is not
// /btw, which is the other direction — a note for the model that expects no
// reply.

// askMaxTokens caps the answer. This is a side question, not a turn; an
// unbounded one would sit in front of the work it interrupted.
const askMaxTokens = 1024

// askPreamble tells the model what kind of answer this is. Without it the
// model tends to reply as though it were resuming the task, which is exactly
// what this is not.
const askPreamble = "Answer this question directly and briefly, from what you already know of " +
	"this conversation. Do not use tools, do not propose changes, and do not resume the task — " +
	"this is a side question and your answer is shown once and discarded.\n\nQuestion: "

// askState is an in-flight side question.
type askState struct {
	question string
	answer   strings.Builder
	done     bool
	err      string
	cancel   context.CancelFunc
	ch       chan askEvent
}

type askEvent struct {
	text string
	err  error
	done bool
}

type askMsg askEvent

// cmdAsk starts a side question. The conversation is copied, never appended
// to: the whole point is that the history is the same afterwards.
func cmdAsk(m *App, arg string) tea.Cmd {
	a := m.mgr.ActiveAgent()
	if a == nil {
		return nil
	}
	q := strings.TrimSpace(arg)
	if q == "" {
		m.setFlash("usage: /ask <question> — answered from context, kept out of the history", true)
		return m.flashCmd()
	}
	if a.Provider == nil {
		m.setFlash("no provider — /connect first", true)
		return m.flashCmd()
	}
	if a.Model == "" {
		m.setFlash("no model selected — /model picks one", true)
		return m.flashCmd()
	}

	// A copy, with the question on the end. a.Messages is untouched.
	history := make([]provider.Message, len(a.Messages), len(a.Messages)+1)
	copy(history, a.Messages)
	history = append(history, provider.TextMessage("user", askPreamble+q))

	ctx, cancel := context.WithCancel(context.Background())
	st := &askState{question: q, cancel: cancel, ch: make(chan askEvent, 64)}
	m.ask = st
	m.overlay = overlayAsk

	req := provider.Request{
		Model:     a.Model,
		System:    a.SystemPrompt,
		Messages:  agent.RepairHistory(history),
		MaxTokens: askMaxTokens,
		// No tools: an answer that edits files is not a side question. No
		// thinking either — this is meant to come back quickly.
	}
	prov := a.Provider

	// One goroutine owns st.ch and closes it. Forwarding deltas from a second
	// one raced the final send against the close and panicked — the provider
	// contract is that Stream closes its event channel before returning, so
	// draining it here is enough.
	go func() {
		defer close(st.ch)
		events := make(chan provider.Event, 64)
		streamErr := make(chan error, 1)
		go func() {
			_, err := prov.Stream(ctx, req, events)
			streamErr <- err
		}()
		for ev := range events {
			if ev.Type != provider.EventTextDelta || ev.Text == "" {
				continue
			}
			select {
			case st.ch <- askEvent{text: ev.Text}:
			case <-ctx.Done():
				return
			}
		}
		select {
		case st.ch <- askEvent{err: <-streamErr, done: true}:
		case <-ctx.Done():
		}
	}()

	return tea.Batch(waitAsk(st), m.flashCmd())
}

// waitAsk pulls one event, the same way the agent stream is pumped.
func waitAsk(st *askState) tea.Cmd {
	ch := st.ch
	return func() tea.Msg {
		ev, ok := <-ch
		if !ok {
			return askMsg{done: true}
		}
		return askMsg(ev)
	}
}

// handleAsk folds a streamed chunk into the overlay.
func (m *App) handleAsk(msg askMsg) tea.Cmd {
	if m.ask == nil {
		return nil
	}
	if msg.text != "" {
		m.ask.answer.WriteString(msg.text)
	}
	if msg.err != nil {
		m.ask.err = sanitizeLabel(msg.err.Error())
	}
	if msg.done {
		m.ask.done = true
		return nil
	}
	return waitAsk(m.ask)
}

// closeAsk dismisses the overlay and stops the request behind it.
func (m *App) closeAsk() {
	if m.ask != nil && m.ask.cancel != nil {
		m.ask.cancel()
	}
	m.ask = nil
	m.overlay = overlayNone
}

// viewAsk renders the question and whatever has arrived.
func (m *App) viewAsk() string {
	if m.ask == nil {
		return ""
	}
	w := min(m.w-8, 88)
	var b strings.Builder
	b.WriteString(styAccent.Render("ask") + "\n")
	b.WriteString(styDim.Render(truncate(m.ask.question, w-4)) + "\n\n")

	switch {
	case m.ask.err != "":
		b.WriteString(styErr.Render(gl.toolErr + " " + m.ask.err))
	case m.ask.answer.Len() == 0:
		b.WriteString(styDim.Render("thinking…"))
	default:
		b.WriteString(wrapPlain(sanitizeText(m.ask.answer.String()), w-4))
	}
	if !zenMode {
		hint := "esc closes"
		if !m.ask.done {
			hint = "esc stops and closes"
		}
		b.WriteString("\n\n" + styDim.Render(hint+gl.sep+"nothing here is added to the conversation"))
	}
	return borderStyle(true).Width(w).Padding(0, 1).Render(b.String())
}
