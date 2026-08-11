package agent

import (
	"encoding/json"
	"fmt"
	"strings"

	"tapioca/internal/provider"
	"tapioca/internal/session"
)

// Todo statuses, in the order they progress.
const (
	TodoPending = "pending"
	TodoDoing   = "in_progress"
	TodoDone    = "completed"
)

// TodoItem is one entry of an agent's plan for a multi-step task. The type is
// defined in session so saved workspaces can carry it without an import cycle.
type TodoItem = session.TodoItem

// TodoTool is offered alongside the executor's tools. It is not a permission
// concern: it only writes the agent's own plan, never the filesystem.
var TodoTool = provider.ToolDef{
	Name: "todo_write",
	Description: "Record or update your plan for a multi-step task. Send the whole list every time; " +
		"it replaces the previous one. Keep exactly one item in_progress, mark items completed as " +
		"you finish them, and skip this tool for single-step work.",
	InputSchema: json.RawMessage(`{"type":"object","properties":{"todos":{"type":"array","items":{"type":"object","properties":{"content":{"type":"string"},"status":{"type":"string","enum":["pending","in_progress","completed"]}},"required":["content","status"]}}},"required":["todos"]}`),
}

func normalizeTodoStatus(s string) string {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case TodoDoing, "doing", "active", "in-progress", "started":
		return TodoDoing
	case TodoDone, "done", "complete", "finished":
		return TodoDone
	default:
		return TodoPending
	}
}

// writeTodos replaces the agent's plan and reports it back so the model sees
// the state it just set.
// stripControls removes what a terminal would act on: C0, DEL and C1.
func stripControls(s string) string {
	return strings.Map(func(r rune) rune {
		if (r < 0x20 && r != '\n' && r != '\t') || r == 0x7f || (r >= 0x80 && r <= 0x9f) {
			return -1
		}
		return r
	}, s)
}

func (a *Agent) writeTodos(raw json.RawMessage) (string, bool) {
	var in struct {
		Todos []TodoItem `json:"todos"`
	}
	if err := json.Unmarshal(raw, &in); err != nil {
		return "invalid arguments: need {\"todos\": [{\"content\", \"status\"}]}", true
	}
	items := make([]TodoItem, 0, len(in.Todos))
	for _, t := range in.Todos {
		// Drawn in the plan panel, so control characters are stripped here
		// rather than at each place that renders one.
		if content := stripControls(strings.TrimSpace(t.Content)); content != "" {
			items = append(items, TodoItem{Content: content, Status: normalizeTodoStatus(t.Status)})
		}
	}
	a.Todos = items
	if len(items) == 0 {
		return "todo list cleared", false
	}
	var b strings.Builder
	done := 0
	for _, t := range items {
		mark := " "
		switch t.Status {
		case TodoDone:
			mark, done = "x", done+1
		case TodoDoing:
			mark = ">"
		}
		fmt.Fprintf(&b, "[%s] %s\n", mark, t.Content)
	}
	fmt.Fprintf(&b, "(%d/%d done)", done, len(items))
	return b.String(), false
}

// TodoProgress reports finished and total items.
func (a *Agent) TodoProgress() (done, total int) {
	for _, t := range a.Todos {
		if t.Status == TodoDone {
			done++
		}
	}
	return done, len(a.Todos)
}
