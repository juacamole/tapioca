package acp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"tapioca/internal/agent"
	"tapioca/internal/tools"
)

// toolKinds maps built-in tools onto ACP's kind vocabulary, which editors use
// to pick an icon and to decide how alarming a call looks.
var toolKinds = map[string]string{
	"bash":        "execute",
	"read_file":   "read",
	"grep":        "search",
	"glob":        "search",
	"write_file":  "edit",
	"edit_file":   "edit",
	"web_search":  "fetch",
	"web_fetch":   "fetch",
	"todo_write":  "think",
	"spawn_agent": "other",
}

func toolKind(name string) string {
	if k, ok := toolKinds[name]; ok {
		return k
	}
	return "other"
}

func (s *Server) update(sessionID string, update map[string]any) {
	_ = s.conn.notify("session/update", map[string]any{
		"sessionId": sessionID,
		"update":    update,
	})
}

func textBlock(text string) map[string]any {
	return map[string]any{"type": "text", "text": text}
}

// pump forwards one turn's events to the client and returns the ACP stop
// reason. It ends on EvDone, on cancellation, or if the agent channel closes.
func (s *Server) pump(ctx context.Context, sess *acpSession) string {
	a := sess.agent
	calls := map[string]string{} // tool name -> most recent call id
	seq := 0

	for {
		select {
		case <-ctx.Done():
			a.Cancel()
			// Drain until the agent acknowledges, so the next prompt starts
			// from a settled state rather than mid-stream.
			for ev := range a.Events {
				if ev.Kind == agent.EvDone {
					break
				}
			}
			return "cancelled"

		case ev, ok := <-a.Events:
			if !ok {
				return "end_turn"
			}
			switch ev.Kind {
			case agent.EvTextDelta:
				s.update(sess.id, map[string]any{
					"sessionUpdate": "agent_message_chunk",
					"content":       textBlock(ev.Text),
				})
			case agent.EvThinkingDelta:
				s.update(sess.id, map[string]any{
					"sessionUpdate": "agent_thought_chunk",
					"content":       textBlock(ev.Text),
				})
			case agent.EvToolStart:
				if ev.Tool == nil {
					continue
				}
				seq++
				id := fmt.Sprintf("call_%d", seq)
				calls[ev.Tool.Name] = id
				s.update(sess.id, map[string]any{
					"sessionUpdate": "tool_call",
					"toolCallId":    id,
					"title":         toolTitle(ev.Tool),
					"kind":          toolKind(ev.Tool.Name),
					"status":        "in_progress",
				})
			case agent.EvToolEnd:
				if ev.Tool == nil {
					continue
				}
				id := calls[ev.Tool.Name]
				if id == "" {
					seq++
					id = fmt.Sprintf("call_%d", seq)
				}
				status := "completed"
				if ev.Tool.IsError {
					status = "failed"
				}
				s.update(sess.id, map[string]any{
					"sessionUpdate": "tool_call_update",
					"toolCallId":    id,
					"status":        status,
					"content": []any{map[string]any{
						"type":    "content",
						"content": textBlock(ev.Tool.Result),
					}},
				})
				if ev.Tool.Name == agent.TodoTool.Name {
					s.sendPlan(sess)
				}
			case agent.EvPermission:
				if ev.Perm != nil {
					s.askPermission(ctx, sess, ev.Perm)
				}
			case agent.EvSpawn:
				// ACP has no place to put a second agent's tabs, and an
				// unanswered request would block the turn forever.
				if ev.Spawn != nil {
					ev.Spawn.Reply <- agent.SpawnResult{
						Err: errors.New("delegation is not available over ACP; do the work in this session"),
					}
				}
			case agent.EvUsage:
				if ev.Usage != nil {
					s.update(sess.id, map[string]any{
						"sessionUpdate": "usage_update",
						"used":          ev.Usage.InputTokens + ev.Usage.OutputTokens,
					})
				}
			case agent.EvError:
				if ev.Err != nil {
					s.update(sess.id, map[string]any{
						"sessionUpdate": "agent_message_chunk",
						"content":       textBlock("error: " + ev.Err.Error()),
					})
				}
			case agent.EvDone:
				return "end_turn"
			}
		}
	}
}

// sendPlan mirrors the agent's todo list into ACP's plan view.
func (s *Server) sendPlan(sess *acpSession) {
	todos := sess.agent.TodoList()
	if len(todos) == 0 {
		return
	}
	entries := make([]any, 0, len(todos))
	for _, t := range todos {
		entries = append(entries, map[string]any{
			"content":  t.Content,
			"priority": "medium",
			"status":   t.Status, // pending | in_progress | completed, as ACP uses
		})
	}
	s.update(sess.id, map[string]any{"sessionUpdate": "plan", "entries": entries})
}

// askPermission turns a tool permission request into ACP's client-side prompt
// and feeds the answer back to the blocked agent.
func (s *Server) askPermission(ctx context.Context, sess *acpSession, req *agent.PermissionReq) {
	seqID := fmt.Sprintf("perm_%s", req.Tool)
	res, err := s.conn.call(ctx, "session/request_permission", map[string]any{
		"sessionId": sess.id,
		"toolCall": map[string]any{
			"toolCallId": seqID,
			"title":      req.Tool + ": " + req.Summary,
			"kind":       toolKind(req.Tool),
			"status":     "pending",
		},
		"options": []any{
			map[string]any{"optionId": "allow-once", "name": "Allow once", "kind": "allow_once"},
			map[string]any{"optionId": "allow-always", "name": "Always allow", "kind": "allow_always"},
			map[string]any{"optionId": "reject", "name": "Reject", "kind": "reject_once"},
		},
	})
	decision := tools.Decision{}
	if err == nil {
		var out struct {
			Outcome struct {
				Outcome  string `json:"outcome"`
				OptionID string `json:"optionId"`
			} `json:"outcome"`
		}
		if json.Unmarshal(res, &out) == nil && out.Outcome.Outcome == "selected" {
			switch out.Outcome.OptionID {
			case "allow-once":
				decision = tools.Decision{Allow: true}
			case "allow-always":
				decision = tools.Decision{Allow: true, Always: true}
			}
		}
	}
	select {
	case req.Reply <- decision:
	default:
	}
}

func toolTitle(t *agent.ToolInfo) string {
	if t.Args == "" || t.Args == "{}" {
		return t.Name
	}
	args := t.Args
	if len(args) > 200 {
		args = args[:200] + "…"
	}
	return t.Name + " " + args
}
