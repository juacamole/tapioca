package ui

import (
	"strings"
	"testing"

	"tapioca/internal/agent"
	"tapioca/internal/provider"
)

func queueApp(t *testing.T, prompts ...string) (*App, *agent.Agent) {
	t.Helper()
	m := dashApp(t, 100, 30)
	a := m.mgr.ActiveAgent()
	for _, p := range prompts {
		msg := provider.TextMessage("user", p)
		msg.Typed = p
		a.Queue = append(a.Queue, msg)
	}
	return m, a
}

// The queue could only be appended to, popped from the front, or cleared
// entirely. Dropping one entry is the thing that was missing.
func TestDropOneQueuedPromptLeavesTheOthers(t *testing.T) {
	m, a := queueApp(t, "first", "second", "third")

	m.dropQueued(1)

	if len(a.Queue) != 2 {
		t.Fatalf("queue has %d entries, want 2", len(a.Queue))
	}
	if queueLabel(a.Queue[0]) != "first" || queueLabel(a.Queue[1]) != "third" {
		t.Errorf("dropped the wrong one: %q, %q", queueLabel(a.Queue[0]), queueLabel(a.Queue[1]))
	}
}

// An index outside the queue must do nothing rather than panic — the picker is
// rebuilt from a queue that can drain while it is open.
func TestDropOutsideTheQueueIsHarmless(t *testing.T) {
	m, a := queueApp(t, "only")
	for _, idx := range []int{-1, 1, 99} {
		m.dropQueued(idx)
	}
	if len(a.Queue) != 1 {
		t.Errorf("queue changed to %d entries", len(a.Queue))
	}
}

// Pulling a prompt back has to remove it: leaving it queued would send the old
// text as well as the corrected one, which is worse than either alone.
func TestEditingAQueuedPromptRemovesItFromTheQueue(t *testing.T) {
	m, a := queueApp(t, "add tests", "update the README")

	m.applyQueuePick("0")

	if len(a.Queue) != 1 {
		t.Fatalf("queue has %d entries, want 1", len(a.Queue))
	}
	if queueLabel(a.Queue[0]) != "update the README" {
		t.Errorf("the wrong prompt was pulled back; %q remains", queueLabel(a.Queue[0]))
	}
	if got := m.ta.Value(); got != "add tests" {
		t.Errorf("the input holds %q, want the pulled-back prompt", got)
	}
}

// The label is what the user typed, before mention expansion — that is what
// they will recognise in a list.
func TestQueueLabelPrefersWhatWasTyped(t *testing.T) {
	msg := provider.TextMessage("user", "look at <expanded contents of main.go>")
	msg.Typed = "look at @main.go"
	if got := queueLabel(msg); got != "look at @main.go" {
		t.Errorf("label = %q, want the typed form", got)
	}
}

// Steering goes to the front, so it is the next thing sent rather than the
// last — otherwise it is just another queued prompt.
func TestSteerJumpsTheQueue(t *testing.T) {
	m, a := queueApp(t, "queued first", "queued second")
	a.Status = agent.StatusStreaming

	cmdSteer(m, "actually do this instead")

	if len(a.Queue) != 3 {
		t.Fatalf("queue has %d entries, want 3", len(a.Queue))
	}
	if got := queueLabel(a.Queue[0]); got != "actually do this instead" {
		t.Errorf("front of the queue is %q, want the steer", got)
	}
	if queueLabel(a.Queue[1]) != "queued first" {
		t.Error("steering reordered the prompts behind it")
	}
}

// With nothing running there is nothing to steer, so it is just a prompt —
// queueing it would leave it sitting there with no turn to end.
func TestSteerWithAnIdleAgentSendsImmediately(t *testing.T) {
	m, a := queueApp(t)
	a.Status = agent.StatusIdle
	a.Provider = &stubProvider{name: "stub", reply: "ok"}
	a.Model = "stub-model"

	cmdSteer(m, "do the thing")

	if len(a.Queue) != 0 {
		t.Errorf("an idle agent queued the steer instead of sending it: %d waiting", len(a.Queue))
	}
}

// An empty steer is a usage message, not a cancelled turn.
func TestEmptySteerDoesNotCancelAnything(t *testing.T) {
	m, a := queueApp(t)
	a.Status = agent.StatusStreaming

	cmdSteer(m, "   ")

	if len(a.Queue) != 0 {
		t.Error("an empty steer queued something")
	}
	if !strings.Contains(m.flash, "usage") {
		t.Errorf("no usage message: %q", m.flash)
	}
}

// Opening the picker with nothing queued should say so rather than show an
// empty list.
func TestQueuePickerWithNothingQueued(t *testing.T) {
	m, _ := queueApp(t)
	m.openQueuePicker()
	if m.overlay == overlayPicker {
		t.Error("an empty queue opened a picker")
	}
	if !strings.Contains(m.flash, "nothing queued") {
		t.Errorf("flash = %q", m.flash)
	}
}
