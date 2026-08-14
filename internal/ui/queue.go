package ui

import (
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"

	"tapioca/internal/provider"
)

// A prompt submitted while the agent was working could only be queued and then
// waited for. If the agent self-corrected a moment later the queued prompt was
// wrong, and there was no way to remove it, edit it, or say "actually, do this
// now" — the queue supported append, pop-front and clear-all and nothing else.

// queueLabel is what a queued prompt shows as. Typed is the raw input before
// mention expansion, which is what the user actually wrote and so what they
// will recognise in a list.
func queueLabel(msg provider.Message) string {
	if t := strings.TrimSpace(msg.Typed); t != "" {
		return t
	}
	return strings.TrimSpace(msg.Text())
}

// openQueuePicker lists the queued prompts so one can be pulled back or
// dropped.
func (m *App) openQueuePicker() {
	a := m.mgr.ActiveAgent()
	if a == nil || len(a.Queue) == 0 {
		m.setFlash("nothing queued", false)
		return
	}
	items := make([]pickerItem, 0, len(a.Queue))
	for i, q := range a.Queue {
		label := queueLabel(q)
		items = append(items, pickerItem{
			label:  fmt.Sprintf("%d. %s", i+1, truncate(label, 60)),
			desc:   "enter edits" + gl.sep + "d drops it",
			value:  fmt.Sprintf("%d", i),
			search: label,
		})
	}
	m.pick = newPicker(pickQueue, "queued prompts", items)
	m.overlay = overlayPicker
}

// dropQueued removes one queued prompt. Index rather than identity because two
// identical prompts are two entries and dropping "the first one" has to mean
// the one that was selected.
func (m *App) dropQueued(idx int) {
	a := m.mgr.ActiveAgent()
	if a == nil || idx < 0 || idx >= len(a.Queue) {
		return
	}
	a.Queue = append(a.Queue[:idx], a.Queue[idx+1:]...)
	m.dirty = true
	m.refreshChat(true)
}

// applyQueuePick pulls a queued prompt back into the input for editing. It is
// removed from the queue first: leaving it there would send the old text as
// well as the corrected one, which is worse than either.
func (m *App) applyQueuePick(value string) tea.Cmd {
	a := m.mgr.ActiveAgent()
	if a == nil {
		return nil
	}
	idx := -1
	if _, err := fmt.Sscanf(value, "%d", &idx); err != nil || idx < 0 || idx >= len(a.Queue) {
		return nil
	}
	text := queueLabel(a.Queue[idx])
	m.dropQueued(idx)
	m.ta.SetValue(text)
	m.ta.CursorEnd()
	m.focus = focusInput
	m.recalcLayout()
	m.setFlash("pulled back for editing"+gl.sep+fmt.Sprintf("%d still queued", len(a.Queue)), false)
	return tea.Batch(m.ta.Focus(), m.flashCmd())
}

// cmdQueue opens the queue.
func cmdQueue(m *App, _ string) tea.Cmd {
	m.openQueuePicker()
	return m.flashCmd()
}

// cmdSteer stops the current turn and sends this instead, keeping whatever the
// agent had already produced.
//
// Cancelling preserves the partial reply — the run loop emits it rather than
// discarding it — so steering costs the work in flight but not the work done.
// The message goes to the front of the queue and the existing drain sends it
// when the turn ends, which means one path sends prompts rather than two.
func cmdSteer(m *App, arg string) tea.Cmd {
	a := m.mgr.ActiveAgent()
	if a == nil {
		return nil
	}
	text := strings.TrimSpace(arg)
	if text == "" {
		m.setFlash("usage: /steer <what to do instead> — stops the turn, keeps what it has", true)
		return m.flashCmd()
	}
	msg := buildUserMessage(m.expandMentions(text, &m.pending), m.pending, text)
	m.pending = nil

	if !a.Status.Busy() {
		// Nothing to steer; this is just a prompt.
		return m.sendPrepared(a, msg)
	}
	a.Queue = append([]provider.Message{msg}, a.Queue...)
	a.Cancel()
	m.dirty = true
	m.refreshChat(true)
	m.setFlash("steering"+gl.sep+"stopping this turn and sending yours next", false)
	return m.flashCmd()
}
