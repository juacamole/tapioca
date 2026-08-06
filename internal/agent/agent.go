// Package agent runs conversations: it streams model responses, executes
// built-in and MCP tool calls in a loop, and reports everything to the UI as
// events.
package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"tapioca/internal/mcp"
	"tapioca/internal/provider"
	"tapioca/internal/stats"
	"tapioca/internal/tools"
)

// Status is the agent's lifecycle state, shown in dashboards.
type Status int

const (
	StatusIdle Status = iota
	StatusWaiting
	StatusThinking
	StatusStreaming
	StatusTool
	StatusError
)

// String returns a short label for the status.
func (s Status) String() string {
	switch s {
	case StatusWaiting:
		return "waiting"
	case StatusThinking:
		return "thinking"
	case StatusStreaming:
		return "streaming"
	case StatusTool:
		return "running tool"
	case StatusError:
		return "error"
	default:
		return "idle"
	}
}

// Busy reports whether the agent is mid-turn.
func (s Status) Busy() bool {
	return s == StatusWaiting || s == StatusThinking || s == StatusStreaming || s == StatusTool
}

// EventKind enumerates agent events delivered to the UI.
type EventKind int

const (
	EvStatus EventKind = iota
	EvTextDelta
	EvThinkingDelta
	EvMessage // a finished message to append (assistant or tool results)
	EvToolStart
	EvToolEnd
	EvUsage
	EvPermission // a built-in tool wants permission; answer via Perm.Reply
	EvError
	EvDone // turn finished (after any tool rounds)
)

// ToolInfo describes a tool call in events.
type ToolInfo struct {
	Name    string
	Args    string
	Result  string
	IsError bool
	Dur     time.Duration
}

// PermissionReq asks the user to allow a built-in tool call. Exactly one
// Decision must be sent on Reply.
type PermissionReq struct {
	Tool    string
	Summary string
	Reply   chan tools.Decision
}

// Event is one update from a running agent.
type Event struct {
	AgentID int
	Kind    EventKind
	Text    string
	Status  Status
	Tool    *ToolInfo
	Usage   *provider.Usage
	Model   string
	Dur     time.Duration
	Message *provider.Message
	Perm    *PermissionReq
	Err     error
}

const (
	maxToolRounds     = 40
	toolCallTimeout   = 3 * time.Minute
	maxToolResultSize = 30_000
)

// Agent is one independent conversation with its own provider, prompt,
// history and stats. Messages, Stats and the Stream* fields are owned by the
// UI goroutine and mutated only in response to events.
type Agent struct {
	ID           int
	Name         string
	ProviderName string
	Provider     provider.Provider
	ProviderErr  string // set when the provider could not be constructed

	Model          string
	SystemPrompt   string
	Goal           string // session goal appended to the system prompt (/goal)
	MaxTokens      int
	Temperature    float64
	Thinking       bool
	ThinkingBudget int
	ToolsEnabled   bool

	Messages     []provider.Message
	Queue        []string // prompts queued while the agent is busy
	Stats        stats.Stats
	Status       Status
	StatusDetail string // fine-grained stage label ("reasoning", "running bash", …)
	LastErr      string
	CtxTokens    int // last known context size (input+output of last request)

	// Live streaming buffers for the in-flight assistant message.
	StreamText     string
	StreamThinking string
	StreamStart    time.Time

	Events chan Event
	MCP    *mcp.Registry
	Exec   *tools.Executor
	cancel context.CancelFunc
}

// Cancel aborts the in-flight turn, if any.
func (a *Agent) Cancel() {
	if a.cancel != nil {
		a.cancel()
	}
}

// TotalTokens returns lifetime in+out tokens.
func (a *Agent) TotalTokens() int {
	return a.Stats.InputTokens + a.Stats.OutputTokens
}

// System composes the effective system prompt (base + goal + cwd + mode).
func (a *Agent) System() string {
	sys := a.SystemPrompt
	if a.Goal != "" {
		sys += "\n\nCurrent goal: " + a.Goal
	}
	if a.Exec != nil {
		sys += "\n\nWorking directory: " + a.Exec.Cwd()
		if a.Exec.Mode() == tools.ModePlan {
			sys += "\n\nPLAN MODE is active: you may inspect the codebase with read-only " +
				"tools, but you must NOT modify files or run mutating commands. " +
				"Investigate, then present a concise implementation plan and wait for approval."
		}
	}
	return sys
}

// Send starts a turn using history (which must already include the new user
// message). It returns immediately; progress arrives on a.Events.
func (a *Agent) Send(history []provider.Message) {
	ctx, cancel := context.WithCancel(context.Background())
	a.cancel = cancel
	go a.run(ctx, history)
}

func (a *Agent) emit(ev Event) {
	ev.AgentID = a.ID
	a.Events <- ev
}

func (a *Agent) run(ctx context.Context, history []provider.Message) {
	defer a.emit(Event{Kind: EvDone})

	if a.Provider == nil {
		msg := a.ProviderErr
		if msg == "" {
			msg = "no provider configured"
		}
		a.emit(Event{Kind: EvError, Err: errors.New(msg)})
		return
	}

	ask := func(tool, summary string) tools.Decision {
		req := &PermissionReq{Tool: tool, Summary: summary, Reply: make(chan tools.Decision, 1)}
		a.emit(Event{Kind: EvPermission, Perm: req})
		select {
		case d := <-req.Reply:
			return d
		case <-ctx.Done():
			return tools.Decision{}
		}
	}

	for round := 0; round < maxToolRounds; round++ {
		stage := "processing prompt"
		if round > 0 {
			stage = "processing tool results"
		}
		a.emit(Event{Kind: EvStatus, Status: StatusWaiting, Text: stage})

		req := provider.Request{
			Model:          a.Model,
			System:         a.System(),
			Messages:       history,
			MaxTokens:      a.MaxTokens,
			Temperature:    a.Temperature,
			Thinking:       a.Thinking,
			ThinkingBudget: a.ThinkingBudget,
		}
		if a.ToolsEnabled {
			if a.Exec != nil {
				req.Tools = append(req.Tools, a.Exec.Tools()...)
			}
			if a.MCP != nil {
				req.Tools = append(req.Tools, a.MCP.AllTools()...)
			}
		}

		events := make(chan provider.Event, 64)
		var msg provider.Message
		var streamErr error
		done := make(chan struct{})
		start := time.Now()
		go func() {
			msg, streamErr = a.Provider.Stream(ctx, req, events)
			close(done)
		}()
		sawThinking, sawText := false, false
		for ev := range events {
			switch ev.Type {
			case provider.EventTextDelta:
				if !sawText {
					sawText = true
					a.emit(Event{Kind: EvStatus, Status: StatusStreaming, Text: "writing"})
				}
				a.emit(Event{Kind: EvTextDelta, Text: ev.Text})
			case provider.EventThinkingDelta:
				if !sawThinking {
					sawThinking = true
					a.emit(Event{Kind: EvStatus, Status: StatusThinking, Text: "reasoning"})
				}
				a.emit(Event{Kind: EvThinkingDelta, Text: ev.Text})
			case provider.EventThinkingDone:
				if !sawText {
					a.emit(Event{Kind: EvStatus, Status: StatusThinking, Text: "preparing answer"})
				}
			case provider.EventToolUseStart:
				a.emit(Event{Kind: EvStatus, Status: StatusStreaming, Text: "planning tool call: " + ev.ToolName})
			}
		}
		<-done
		dur := time.Since(start)

		cancelled := errors.Is(streamErr, context.Canceled)
		if streamErr != nil && !cancelled {
			a.emit(Event{Kind: EvError, Err: streamErr})
			return
		}
		if msg.Usage != nil {
			a.emit(Event{Kind: EvUsage, Usage: msg.Usage, Model: a.Model, Dur: dur})
		}
		if len(msg.Blocks) > 0 {
			m := msg
			a.emit(Event{Kind: EvMessage, Message: &m})
			history = append(history, m)
		}
		toolUses := msg.ToolUses()
		if cancelled {
			if len(toolUses) > 0 {
				a.emitToolResults(interruptedResults(toolUses))
			}
			return
		}
		if len(toolUses) == 0 {
			return
		}

		var results []provider.Block
		interrupted := false
		for _, tu := range toolUses {
			if ctx.Err() != nil {
				results = append(results, interruptedResult(tu))
				interrupted = true
				continue
			}
			argsPreview := compactJSON(tu.Input)
			a.emit(Event{Kind: EvStatus, Status: StatusTool})
			a.emit(Event{Kind: EvToolStart, Tool: &ToolInfo{Name: tu.Name, Args: argsPreview}})

			var (
				text    string
				isErr   bool
				callErr error
			)
			toolStart := time.Now()
			callCtx, cancelCall := context.WithTimeout(ctx, toolCallTimeout)
			switch {
			case a.Exec != nil && a.Exec.Has(tu.Name):
				text, isErr, callErr = a.Exec.Call(callCtx, tu.Name, tu.Input, ask)
			case a.MCP != nil:
				text, isErr, callErr = a.MCP.Call(callCtx, tu.Name, tu.Input)
			default:
				callErr = fmt.Errorf("no handler for tool %q", tu.Name)
			}
			cancelCall()
			tdur := time.Since(toolStart)
			if callErr != nil {
				text = "tool error: " + callErr.Error()
				isErr = true
			}
			if len(text) > maxToolResultSize {
				text = text[:maxToolResultSize] + "\n[truncated]"
			}
			a.emit(Event{Kind: EvToolEnd, Tool: &ToolInfo{Name: tu.Name, Args: argsPreview, Result: text, IsError: isErr, Dur: tdur}})
			results = append(results, provider.Block{
				Type:      "tool_result",
				ToolUseID: tu.ID,
				Name:      tu.Name,
				Content:   text,
				IsError:   isErr,
			})
		}
		toolMsg := a.emitToolResults(results)
		if interrupted {
			return
		}
		history = append(history, toolMsg)
	}
	a.emit(Event{Kind: EvError, Err: fmt.Errorf("stopped after %d tool rounds", maxToolRounds)})
}

// Every tool_use must get a tool_result, even on cancellation — providers
// reject histories with dangling tool calls.
func interruptedResult(tu provider.Block) provider.Block {
	return provider.Block{
		Type:      "tool_result",
		ToolUseID: tu.ID,
		Name:      tu.Name,
		Content:   "[tool execution was interrupted]",
		IsError:   true,
	}
}

func interruptedResults(toolUses []provider.Block) []provider.Block {
	out := make([]provider.Block, 0, len(toolUses))
	for _, tu := range toolUses {
		out = append(out, interruptedResult(tu))
	}
	return out
}

func (a *Agent) emitToolResults(results []provider.Block) provider.Message {
	toolMsg := provider.Message{Role: "user", Blocks: results, Time: time.Now()}
	a.emit(Event{Kind: EvMessage, Message: &toolMsg})
	return toolMsg
}

func compactJSON(raw json.RawMessage) string {
	if len(raw) == 0 {
		return "{}"
	}
	return string(raw)
}
