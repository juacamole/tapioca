// Package agent runs conversations: it streams model responses, executes
// built-in and MCP tool calls in a loop, and reports everything to the UI as
// events.
package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"tapioca/internal/mcp"
	"tapioca/internal/project"
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
	EvRetry      // transient provider failure; retrying after Delay
	EvNotice     // non-fatal warning for the user
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
	AgentID  int
	Kind     EventKind
	Text     string
	Status   Status
	Tool     *ToolInfo
	Usage    *provider.Usage
	Provider string
	Model    string
	Dur      time.Duration
	Message  *provider.Message
	Perm     *PermissionReq
	Err      error
	Attempt  int
	Max      int
	Delay    time.Duration
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

	Messages      []provider.Message
	Queue         []provider.Message // prompts queued while the agent is busy
	PendingNotes  []provider.Message // notices deferred until the turn ends
	MaxToolRounds int                // 0 = default
	CompactFailed bool               // suppress auto-compact until the next good turn
	Stats         stats.Stats
	Status        Status
	StatusDetail  string // fine-grained stage label ("reasoning", "running bash", …)
	LastErr       string
	CtxTokens     int // last known context size (input+output of last request)

	// Live streaming buffers for the in-flight assistant message.
	StreamText     string
	StreamThinking string
	StreamStart    time.Time
	RetryAt        time.Time // when the next retry attempt fires
	RetryNote      string

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

// runSettings is a snapshot of everything the run goroutine needs. The UI
// goroutine owns the Agent fields and may rewrite them mid-turn (/model,
// settings panel, /settings reload); reading them from run would race, and a
// reload can even nil the provider. Snapshotting at Send makes each turn see
// one consistent configuration.
type runSettings struct {
	prov           provider.Provider
	providerName   string
	providerErr    string
	model          string
	systemBase     string
	goal           string
	maxTokens      int
	temperature    float64
	thinking       bool
	thinkingBudget int
	toolsEnabled   bool
	rounds         int
}

func (a *Agent) snapshot() runSettings {
	rounds := a.MaxToolRounds
	if rounds <= 0 {
		rounds = maxToolRounds
	}
	return runSettings{
		prov: a.Provider, providerName: a.ProviderName, providerErr: a.ProviderErr,
		model: a.Model, systemBase: a.SystemPrompt, goal: a.Goal,
		maxTokens: a.MaxTokens, temperature: a.Temperature,
		thinking: a.Thinking, thinkingBudget: a.ThinkingBudget,
		toolsEnabled: a.ToolsEnabled, rounds: rounds,
	}
}

// System composes the effective system prompt (base + goal + cwd + mode).
func (a *Agent) System() string { return composeSystem(a.SystemPrompt, a.Goal, a.Exec) }

func composeSystem(base, goal string, exec *tools.Executor) string {
	sys := base
	if goal != "" {
		sys += "\n\nCurrent goal: " + goal
	}
	if exec != nil {
		cwd := exec.Cwd()
		sys += "\n\nWorking directory: " + cwd
		if extra := exec.ExtraDirs(); len(extra) > 0 {
			sys += "\nAdditional working directories: " + strings.Join(extra, ", ")
		}
		if ins := project.Instructions(cwd); ins != "" {
			sys += "\n\nProject instructions:\n" + ins
		}
		if mem := project.Memory(cwd); mem != "" {
			sys += "\n\nProject memory (remembered facts):\n" + mem
		}
		if exec.Mode() == tools.ModePlan {
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
	history = RepairHistory(history)
	rs := a.snapshot()
	ctx, cancel := context.WithCancel(context.Background())
	a.cancel = cancel
	go a.run(ctx, rs, history)
}

func (a *Agent) emit(ev Event) {
	ev.AgentID = a.ID
	a.Events <- ev
}

func (a *Agent) run(ctx context.Context, rs runSettings, history []provider.Message) {
	defer a.emit(Event{Kind: EvDone})

	if rs.prov == nil {
		msg := rs.providerErr
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

	doomKey, doomCount, doomOff := "", 0, false
	for round := 0; round < rs.rounds; round++ {
		stage := "processing prompt"
		if round > 0 {
			stage = "processing tool results"
		}
		a.emit(Event{Kind: EvStatus, Status: StatusWaiting, Text: stage})

		req := provider.Request{
			Model:          rs.model,
			System:         composeSystem(rs.systemBase, rs.goal, a.Exec),
			Messages:       history,
			MaxTokens:      rs.maxTokens,
			Temperature:    rs.temperature,
			Thinking:       rs.thinking,
			ThinkingBudget: rs.thinkingBudget,
		}
		if rs.toolsEnabled {
			if a.Exec != nil {
				req.Tools = append(req.Tools, a.Exec.Tools()...)
			}
			if a.MCP != nil {
				req.Tools = append(req.Tools, a.MCP.AllTools()...)
			}
		}

		var msg provider.Message
		var streamErr error
		var stopReason string
		start := time.Now()
		for attempt := 1; ; attempt++ {
			events := make(chan provider.Event, 64)
			done := make(chan struct{})
			go func() {
				msg, streamErr = rs.prov.Stream(ctx, req, events)
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
				case provider.EventDone:
					stopReason = ev.StopReason
				}
			}
			<-done
			if streamErr == nil || ctx.Err() != nil || errors.Is(streamErr, context.Canceled) {
				break
			}
			if attempt >= provider.RetryMaxAttempts || !provider.Retryable(streamErr) {
				break
			}
			delay, ok := provider.RetryDelay(attempt, streamErr)
			if !ok {
				break
			}
			a.emit(Event{Kind: EvRetry, Err: streamErr, Attempt: attempt, Max: provider.RetryMaxAttempts, Delay: delay})
			select {
			case <-time.After(delay):
			case <-ctx.Done():
			}
		}
		dur := time.Since(start)

		cancelled := errors.Is(streamErr, context.Canceled) || (streamErr != nil && ctx.Err() != nil)
		if streamErr != nil && !cancelled {
			// Keep whatever partial content survived under the error, and
			// close any dangling tool calls it carries.
			if len(msg.Blocks) > 0 {
				m := msg
				a.emit(Event{Kind: EvMessage, Message: &m})
				if uses := msg.ToolUses(); len(uses) > 0 {
					a.emitToolResults(interruptedResults(uses))
				}
			}
			a.emit(Event{Kind: EvError, Err: streamErr})
			return
		}
		if msg.Usage != nil {
			a.emit(Event{Kind: EvUsage, Usage: msg.Usage, Provider: rs.providerName, Model: rs.model, Dur: dur})
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
		if len(msg.Blocks) == 0 {
			a.emit(Event{Kind: EvError, Err: errors.New("provider returned an empty response — is the model loaded and working?")})
			return
		}
		if stopReason == "length" || stopReason == "max_tokens" {
			a.emit(Event{Kind: EvNotice, Text: "output was cut off at the max tokens limit — raise it in settings, or /regen"})
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

			// Doom loop: three identical calls in a row means the model is
			// stuck; make the user decide instead of burning tokens.
			key := tu.Name + "\x00" + string(tu.Input)
			if key == doomKey {
				doomCount++
			} else {
				doomKey, doomCount = key, 1
			}
			if !doomOff && doomCount >= 3 {
				d := ask("loop: "+tu.Name,
					"the model repeated the exact same call 3 times:\n"+argsPreview+"\nallow it to keep going?")
				switch {
				case d.Always:
					doomOff = true
				case !d.Allow:
					results = append(results, provider.Block{
						Type: "tool_result", ToolUseID: tu.ID, Name: tu.Name, IsError: true,
						Content: "stopped: you repeated the exact same tool call 3 times. Change your approach or ask the user for guidance.",
					})
					doomCount = 0
					continue
				default:
					doomCount = 0
				}
			}

			a.emit(Event{Kind: EvStatus, Status: StatusTool})
			a.emit(Event{Kind: EvToolStart, Tool: &ToolInfo{Name: tu.Name, Args: argsPreview}})

			var (
				text    string
				isErr   bool
				callErr error
			)
			toolStart := time.Now()
			switch {
			case a.Exec != nil && a.Exec.Has(tu.Name):
				// No deadline here: the executor times the execution itself,
				// after any permission prompt has been answered.
				text, isErr, callErr = a.Exec.Call(ctx, tu.Name, tu.Input, ask)
			case a.MCP != nil:
				// MCP tools go through the same permission gate as builtins.
				key := "mcp:" + tu.Name
				allowed := a.Exec == nil || a.Exec.ExternalAllowed(key)
				if !allowed {
					d := ask(key, argsPreview)
					allowed = d.Allow
					if d.Always && a.Exec != nil {
						a.Exec.GrantExternal(key)
					}
				}
				if !allowed {
					text, isErr = "the user denied permission for this call", true
				} else {
					toolStart = time.Now()
					callCtx, cancelCall := context.WithTimeout(ctx, toolCallTimeout)
					text, isErr, callErr = a.MCP.Call(callCtx, tu.Name, tu.Input)
					cancelCall()
				}
			default:
				callErr = fmt.Errorf("no handler for tool %q", tu.Name)
			}
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
	a.emit(Event{Kind: EvError, Err: fmt.Errorf("stopped after %d tool rounds", rs.rounds)})
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
