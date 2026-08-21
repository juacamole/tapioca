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
	"sync"
	"time"

	"tapioca/internal/mcp"
	"tapioca/internal/project"
	"tapioca/internal/provider"
	"tapioca/internal/skills"
	"tapioca/internal/stats"
	"tapioca/internal/textenc"
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
	EvSpawn      // the agent delegated a task; answer via Spawn.Reply
	EvRetry      // transient provider failure; retrying after Delay
	EvFallback   // this model is out; continuing on the next one configured
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
	Change  *tools.FileChange // file edits, for display only
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
	Spawn    *SpawnReq
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
	// Fallbacks are tried in order when this model cannot answer. Resolved by
	// the frontend, which is the only part that can build a provider.
	Fallbacks []fallbackTarget

	Messages      []provider.Message
	Queue         []provider.Message // prompts queued while the agent is busy
	PendingNotes  []provider.Message // notices deferred until the turn ends
	Todos         []TodoItem         // the model's own plan (todo_write)
	Depth         int                // 0 for agents you created; 1 for spawned ones
	CanSpawn      bool               // the frontend can run subagents (TUI only)
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
	// Builders behind the two strings above. Appending to a string on every
	// delta is quadratic in the response length, and the chunk size is the
	// server's choice, so no byte cap bounds the copying.
	StreamBuf   strings.Builder
	ThinkBuf    strings.Builder
	StreamStart time.Time

	// todoMu guards Todos, the one field below that the run goroutine writes
	// directly instead of handing to the UI through emit().
	todoMu    sync.Mutex
	RetryAt   time.Time // when the next retry attempt fires
	RetryNote string

	Events chan Event
	MCP    *mcp.Registry
	Exec   *tools.Executor
	// Remote answers this agent's turns instead of a provider, when it is an
	// external agent Tapioca drives rather than one it runs.
	Remote Remote
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
// fallbackTarget is somewhere to try when the current model cannot answer.
// Resolved before the run starts, because the run loop has no way to build a
// provider from config on its own.
type fallbackTarget struct {
	prov         provider.Provider
	providerName string
	model        string
}

type runSettings struct {
	prov           provider.Provider
	providerName   string
	providerErr    string
	model          string
	fallbacks      []fallbackTarget
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
		model: a.Model, fallbacks: a.Fallbacks, systemBase: a.SystemPrompt, goal: a.Goal,
		maxTokens: a.MaxTokens, temperature: a.Temperature,
		thinking: a.Thinking, thinkingBudget: a.ThinkingBudget,
		toolsEnabled: a.ToolsEnabled, rounds: rounds,
	}
}

// nextFallback moves the run settings to the next configured model and
// reports whether there was one. It mutates rs because everything downstream —
// the request, the usage attribution, the status line — reads the model from
// there, and leaving them disagreeing about which model answered is worse than
// the failure being recovered from.
func nextFallback(rs *runSettings, at *int) (fallbackTarget, bool) {
	if *at+1 >= len(rs.fallbacks) {
		return fallbackTarget{}, false
	}
	*at++
	t := rs.fallbacks[*at]
	rs.prov, rs.providerName, rs.model = t.prov, t.providerName, t.model
	return t, true
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
		// One line per skill and no more: the bodies are what would make this
		// expensive, and load_skill fetches those only when one is relevant.
		if list, _ := skills.Load(cwd); len(list) > 0 {
			sys += "\n\nSkills installed here. Only these descriptions are loaded; " +
				"call load_skill with a name to get the instructions behind it:\n" + skills.Catalog(list)
		}
		// Tool guidance belongs here rather than in the default system prompt:
		// that one is user-editable and already saved in existing configs.
		sys += "\n\nTool notes: use grep and glob to search, not bash grep/find/ls — " +
			"they are faster and run without asking for approval. Read files with " +
			"read_file, not cat. When a task needs several steps, record them with " +
			"todo_write and keep it updated as you go."
		if exec.Mode() == tools.ModePlan {
			sys += "\n\nPLAN MODE is active: you may inspect the codebase with read-only " +
				"tools, but you must NOT modify files or run mutating commands. " +
				"Investigate, then present a concise implementation plan and wait for approval."
		}
	}
	return sys
}

// Remote answers an agent's turns somewhere other than a provider: another
// agent, in its own process, driven over ACP. Declared here rather than in
// internal/acp because that package speaks to this one, not the other way
// round.
type Remote interface {
	// Prompt runs one turn and returns when it has ended, reporting what
	// happened on the way through the agent's own events. A cancelled ctx asks
	// the other side to stop.
	Prompt(ctx context.Context, text string) error
	// Close disconnects and stops the process behind it.
	Close()
}

// Send starts a turn using history (which must already include the new user
// message). It returns immediately; progress arrives on a.Events.
func (a *Agent) Send(history []provider.Message) {
	ctx, cancel := context.WithCancel(context.Background())
	a.cancel = cancel
	if a.Remote != nil {
		// The agent on the other end keeps the conversation; only the new
		// prompt crosses the connection, so history is not replayed to it.
		go a.runRemote(ctx, lastText(history))
		return
	}
	history = RepairHistory(history)
	rs := a.snapshot()
	go a.run(ctx, rs, history)
}

// runRemote drives one turn on an external agent. Whatever goes wrong out
// there is this tab's error and nothing more: a process Tapioca did not write,
// crashing, must not take the session with it.
func (a *Agent) runRemote(ctx context.Context, text string) {
	defer a.emit(Event{Kind: EvDone})
	a.emit(Event{Kind: EvStatus, Status: StatusWaiting, Text: "sending prompt"})
	if err := a.Remote.Prompt(ctx, text); err != nil {
		a.emit(Event{Kind: EvError, Err: err})
	}
}

func lastText(history []provider.Message) string {
	for i := len(history) - 1; i >= 0; i-- {
		if history[i].Role == "user" {
			return history[i].Text()
		}
	}
	return ""
}

// Todos is written by writeTodos on the run goroutine and read by the
// dashboard on the Bubble Tea goroutine — the one Agent field not funnelled
// through emit(). These two are the only way it should be touched.
func (a *Agent) SetTodos(items []TodoItem) {
	a.todoMu.Lock()
	a.Todos = items
	a.todoMu.Unlock()
}

// TodoList returns a snapshot safe to range over.
func (a *Agent) TodoList() []TodoItem {
	a.todoMu.Lock()
	defer a.todoMu.Unlock()
	return append([]TodoItem(nil), a.Todos...)
}

// ResetStream clears the in-flight response and the builders behind it.
func (a *Agent) ResetStream() {
	a.StreamText, a.StreamThinking = "", ""
	a.StreamBuf.Reset()
	a.ThinkBuf.Reset()
}

func (a *Agent) emit(ev Event) {
	ev.AgentID = a.ID
	a.Events <- ev
}

// Emit publishes an event as if this agent's run loop had produced it. An
// agent driven over ACP runs its turn in another process, and the frontend
// should not learn a second vocabulary to display one.
func (a *Agent) Emit(ev Event) { a.emit(ev) }

// Ask puts a permission request in front of the user and blocks for the
// answer. A cancelled turn counts as a refusal: nothing is allowed by the
// absence of an answer.
func (a *Agent) Ask(ctx context.Context, tool, summary string) tools.Decision {
	req := &PermissionReq{Tool: tool, Summary: summary, Reply: make(chan tools.Decision, 1)}
	a.emit(Event{Kind: EvPermission, Perm: req})
	select {
	case d := <-req.Reply:
		return d
	case <-ctx.Done():
		return tools.Decision{}
	}
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

	ask := func(tool, summary string) tools.Decision { return a.Ask(ctx, tool, summary) }

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
			req.Tools = append(req.Tools, TodoTool)
			if a.Depth == 0 && a.CanSpawn {
				req.Tools = append(req.Tools, SpawnTool)
			}
			if a.Exec != nil {
				if list, _ := skills.Load(a.Exec.Cwd()); len(list) > 0 {
					req.Tools = append(req.Tools, SkillTool)
				}
			}
			if a.MCP != nil {
				req.Tools = append(req.Tools, a.MCP.AllTools()...)
			}
		}

		fallbackAt := -1
		var msg provider.Message
		var streamErr error
		var stopReason string
		var lost provider.Usage // usage billed on failed attempts
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
			if !provider.Retryable(streamErr) {
				break
			}
			delay, ok := provider.RetryDelay(attempt, streamErr)
			if attempt >= provider.RetryMaxAttempts || !ok {
				// Retrying this model is finished. The failure classes that
				// get here — rate limits, exhausted quota, a provider having
				// trouble — are rarely about the model, so somewhere else may
				// well answer. A refusal or a bad request never reaches this
				// point: those are answers, and asking a second model would
				// only spend money to be told the same thing twice.
				next, ok := nextFallback(&rs, &fallbackAt)
				if !ok {
					break
				}
				req.Model = rs.model
				a.emit(Event{Kind: EvFallback, Err: streamErr,
					Provider: next.providerName, Model: next.model})
				// The loop's ++ runs before the next pass, and the loop's first
				// attempt is 1 — so this is 0, not -1. At -1 the fallback's
				// first pass was attempt 0, and RetryDelay shifts by
				// attempt-1, which is a panic rather than a delay.
				attempt = 0
				continue
			}
			if msg.Usage != nil {
				lost.InputTokens += msg.Usage.InputTokens
				lost.OutputTokens += msg.Usage.OutputTokens
				lost.CacheReadTokens += msg.Usage.CacheReadTokens
				lost.CacheWriteTokens += msg.Usage.CacheWriteTokens
			}
			a.emit(Event{Kind: EvRetry, Err: streamErr, Attempt: attempt, Max: provider.RetryMaxAttempts, Delay: delay})
			select {
			case <-time.After(delay):
			case <-ctx.Done():
			}
		}
		dur := time.Since(start)

		// Failed and cancelled attempts still billed their input; report
		// everything that was consumed, on every exit path.
		total := lost
		if msg.Usage != nil {
			total.InputTokens += msg.Usage.InputTokens
			total.OutputTokens += msg.Usage.OutputTokens
			total.CacheReadTokens += msg.Usage.CacheReadTokens
			total.CacheWriteTokens += msg.Usage.CacheWriteTokens
		}
		if total != (provider.Usage{}) {
			// The counts stop being something a server said and start being
			// something the dashboard divides by, so this is where they are
			// made believable. See provider.Usage.Clamp.
			u := total.Clamp()
			a.emit(Event{Kind: EvUsage, Usage: &u, Provider: rs.providerName, Model: rs.model, Dur: dur})
		}

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
				change  *tools.FileChange
			)
			toolStart := time.Now()
			switch {
			case tu.Name == TodoTool.Name:
				text, isErr = a.writeTodos(tu.Input)
			case tu.Name == SpawnTool.Name:
				text, isErr = a.spawnAgent(ctx, tu.Input)
			case tu.Name == SkillTool.Name:
				text, isErr = a.loadSkill(tu.Input)
			case a.Exec != nil && a.Exec.Has(tu.Name):
				// No deadline here: the executor times the execution itself,
				// after any permission prompt has been answered.
				var res tools.Result
				res, callErr = a.Exec.CallDetailed(ctx, tu.Name, tu.Input, ask)
				text, isErr, change = res.Text, res.IsErr, res.Change
			case a.MCP != nil:
				// MCP tools go through the same permission gate as builtins,
				// including the configured rules; theirs match on the tool
				// name and the call's arguments.
				key := "mcp:" + tu.Name
				denial := ""
				allowed := true
				if a.Exec != nil {
					denial, allowed = a.Exec.ApproveExternal(key, argsPreview, ask)
				}
				if !allowed {
					text, isErr = denial, true
					break
				}
				// Hooks run here for the same reason the rules do: a policy
				// that covered the built-ins and skipped every tool an MCP
				// server offers would be a policy with a hole in it.
				if a.Exec != nil {
					if reason, blocked := a.Exec.RunPreToolHooks(ctx, key, tu.Input); blocked {
						text, isErr = reason, true
						break
					}
				}
				toolStart = time.Now()
				callCtx, cancelCall := context.WithTimeout(ctx, toolCallTimeout)
				text, isErr, callErr = a.MCP.Call(callCtx, tu.Name, tu.Input)
				cancelCall()
				if a.Exec != nil {
					if note := a.Exec.RunPostToolHooks(ctx, key, tu.Input, isErr || callErr != nil); note != "" {
						text = strings.TrimRight(text, "\n") + "\n" + note
					}
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
				text = textenc.Cut(text, maxToolResultSize) + "\n[truncated]"
			}
			a.emit(Event{Kind: EvToolEnd, Tool: &ToolInfo{Name: tu.Name, Args: argsPreview, Result: text, IsError: isErr, Dur: tdur, Change: change}})
			results = append(results, provider.Block{
				Type:      "tool_result",
				ToolUseID: tu.ID,
				Name:      tu.Name,
				Content:   text,
				IsError:   isErr,
				Display:   tools.FormatChange(change),
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
