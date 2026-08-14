package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"sync"
	"time"

	"tapioca/internal/config"
	"tapioca/internal/secretenv"
)

// The permission gate decides *whether* a tool call happens. A hook is what
// acts when it does: format the file that was just edited, log the command
// that ran, refuse a write no rule covers, say something when the session
// ends.
//
//	[[hooks]]
//	event = "post_tool"
//	match = "edit_file"
//	command = 'gofmt -w "$TAPIOCA_TOOL_PATH"'
//
// A pre_tool hook that exits non-zero blocks the call and its stderr becomes
// the reason. Refusing is the only power it has: hooks run after the gate has
// already approved a call, so a hook narrows what the rules allowed and can
// never widen it — a call a rule denied returns before any hook is consulted,
// and an exit status of 0 is not permission to skip anything.

// Hook events.
const (
	HookPreTool      = "pre_tool"
	HookPostTool     = "post_tool"
	HookSessionStart = "session_start"
	HookSessionEnd   = "session_end"
)

// Hook is one configured command and the point it runs at.
type Hook struct {
	Event   string        // one of the four above
	Match   string        // glob over the tool name; empty matches every tool
	Command string        // run with sh -c, like a bash call
	Timeout time.Duration // 0 = defaultHookTimeout
}

const (
	// A hook sits in the critical path of every tool call, so it gets a
	// deadline: a command waiting on stdin or a dead host would otherwise
	// wedge the session with no way out but killing the app.
	defaultHookTimeout = 30 * time.Second
	maxHookTimeout     = 5 * time.Minute
	// How much a refusing hook is allowed to say, and how much of a call
	// reaches its environment. Linux caps one environment string at 128KB and
	// a model can hand a tool a megabyte of content, so the exact arguments go
	// to stdin as JSON and the variables are a bounded convenience.
	maxHookReason   = 2_000
	maxHookEnvValue = 32 << 10
)

// SetHooks replaces the configured lifecycle hooks, returning a complaint for
// every entry it refused. Silence would be the wrong answer for those: a hook
// naming an event that does not exist is a policy its author believes is in
// force and that never fires.
func (e *Executor) SetHooks(hs []Hook) []string {
	var kept []Hook
	var complaints []string
	for _, h := range hs {
		switch {
		case !validHookEvent(h.Event):
			complaints = append(complaints, fmt.Sprintf("hook with event %q is not one of %s, %s, %s, %s — ignored",
				h.Event, HookPreTool, HookPostTool, HookSessionStart, HookSessionEnd))
			continue
		case strings.TrimSpace(h.Command) == "":
			complaints = append(complaints, "hook for "+h.Event+" has no command — ignored")
			continue
		}
		if h.Timeout > maxHookTimeout {
			h.Timeout = maxHookTimeout
		}
		kept = append(kept, h)
	}
	e.mu.Lock()
	e.hooks = kept
	e.mu.Unlock()
	return complaints
}

// Hooks returns the hooks in force, for /permissions to show.
func (e *Executor) Hooks() []Hook {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]Hook(nil), e.hooks...)
}

// ApplyHooks installs cfg's hooks for work in cwd and returns everything it
// refused. The trust rule lives in the config package; this is the one place
// every frontend goes through to apply it.
func ApplyHooks(e *Executor, cfg *config.Config, cwd string) []string {
	hooks, err := cfg.TrustedHooks(cwd)
	var notes []string
	if err != nil {
		notes = append(notes, err.Error())
	}
	converted := make([]Hook, 0, len(hooks))
	for _, h := range hooks {
		converted = append(converted, Hook{
			Event:   strings.TrimSpace(h.Event),
			Match:   strings.TrimSpace(h.Match),
			Command: h.Command,
			Timeout: time.Duration(h.Timeout) * time.Second,
		})
	}
	return append(notes, e.SetHooks(converted)...)
}

func validHookEvent(ev string) bool {
	switch ev {
	case HookPreTool, HookPostTool, HookSessionStart, HookSessionEnd:
		return true
	}
	return false
}

// hooksFor collects the hooks that fire for one event, in configured order.
func (e *Executor) hooksFor(event, tool string) []Hook {
	e.mu.Lock()
	defer e.mu.Unlock()
	var out []Hook
	for _, h := range e.hooks {
		if h.Event != event {
			continue
		}
		// match names a tool, so it only means anything for the tool events;
		// session hooks fire whatever it says.
		if (event == HookPreTool || event == HookPostTool) && !wildcard(h.Match, tool) {
			continue
		}
		out = append(out, h)
	}
	return out
}

// RunPreToolHooks reports whether a hook refuses this call, and why. It is
// exported because the agent gates MCP tools itself: a policy that covered the
// built-ins and quietly skipped everything an MCP server offers would be a
// policy with a hole in it.
func (e *Executor) RunPreToolHooks(ctx context.Context, tool string, raw json.RawMessage) (string, bool) {
	hooks := e.hooksFor(HookPreTool, tool)
	if len(hooks) == 0 {
		return "", false
	}
	env := e.hookEnv(HookPreTool, tool, raw, false)
	for _, h := range hooks {
		if err := e.runHook(ctx, h, env, string(raw)); err != nil {
			return "blocked by a pre_tool hook (" + truncateLabel(stripControls(h.Command)) + "): " + err.Error(), true
		}
	}
	return "", false
}

// RunPostToolHooks runs the hooks for a finished call and returns a note for
// the tool result when one fails. Their exit status is a report rather than a
// decision — the call already happened — but a formatter that could not run is
// something the model has to be told about, or it moves on believing the file
// was tidied.
func (e *Executor) RunPostToolHooks(ctx context.Context, tool string, raw json.RawMessage, failed bool) string {
	hooks := e.hooksFor(HookPostTool, tool)
	if len(hooks) == 0 {
		return ""
	}
	env := e.hookEnv(HookPostTool, tool, raw, failed)
	var notes []string
	for _, h := range hooks {
		if err := e.runHook(ctx, h, env, string(raw)); err != nil {
			notes = append(notes, "post_tool hook ("+truncateLabel(stripControls(h.Command))+") failed: "+err.Error())
		}
	}
	return strings.Join(notes, "\n")
}

// RunSessionHooks fires the session_start or session_end hooks and returns a
// line for each that failed. Nothing is blocked either way: there is no call to
// refuse, and a session that would not start because a notification command is
// missing is worse than the missing notification.
func (e *Executor) RunSessionHooks(event string) []string {
	hooks := e.hooksFor(event, "")
	if len(hooks) == 0 {
		return nil
	}
	env := e.hookEnv(event, "", nil, false)
	var notes []string
	for _, h := range hooks {
		if err := e.runHook(context.Background(), h, env, ""); err != nil {
			notes = append(notes, event+" hook ("+truncateLabel(stripControls(h.Command))+") failed: "+err.Error())
		}
	}
	return notes
}

// hookEnv describes the call to the hook. The values are capped, so a hook
// that has to be exact about what it is judging reads the JSON on stdin.
func (e *Executor) hookEnv(event, tool string, raw json.RawMessage, failed bool) []string {
	env := []string{
		"TAPIOCA_EVENT=" + event,
		"TAPIOCA_CWD=" + e.Cwd(),
	}
	if tool == "" {
		return env
	}
	env = append(env, "TAPIOCA_TOOL="+capEnv(tool))
	subject := subjectOf(tool, raw)
	switch {
	case tool == "bash":
		env = append(env, "TAPIOCA_TOOL_COMMAND="+capEnv(subject))
	case pathRule(tool) && subject != "":
		// The resolved path, which is what the gate judged and what a
		// formatter needs to open; the relative one the model wrote is in the
		// JSON on stdin.
		env = append(env, "TAPIOCA_TOOL_PATH="+capEnv(e.resolve(subject)))
	}
	if event == HookPostTool {
		errored := "0"
		if failed {
			errored = "1"
		}
		env = append(env, "TAPIOCA_TOOL_ERROR="+errored)
	}
	return env
}

func capEnv(v string) string {
	if len(v) > maxHookEnvValue {
		return v[:maxHookEnvValue]
	}
	return v
}

// runHook executes one hook. The error it returns is what the user and the
// model are shown, so it carries the hook's stderr rather than "exit status 1".
func (e *Executor) runHook(ctx context.Context, h Hook, env []string, stdin string) error {
	timeout := h.Timeout
	if timeout <= 0 {
		timeout = defaultHookTimeout
	}
	hctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	name, args := shellFor(h.Command)
	cmd := exec.CommandContext(hctx, name, args...)
	cmd.Dir = e.Cwd()
	// A hook is a user command like any other, so it gets the environment a
	// bash call gets: provider keys removed.
	cmd.Env = append(secretenv.Scrubbed(), env...)
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	// Output is captured, never inherited: the TUI owns the terminal, and a
	// hook printing to it would scribble over the transcript. Only stderr is
	// kept, since that is what a refusal explains itself with.
	stderr := &capWriter{limit: maxHookReason}
	cmd.Stdout = io.Discard
	cmd.Stderr = stderr
	isolateProcess(cmd)
	cmd.WaitDelay = 5 * time.Second

	err := cmd.Run()
	if err == nil || errors.Is(err, exec.ErrWaitDelay) {
		// ErrWaitDelay means the hook itself finished and something it started
		// still holds the pipe; the exit status was fine.
		return nil
	}
	if hctx.Err() == context.DeadlineExceeded && ctx.Err() == nil {
		return fmt.Errorf("timed out after %s", timeout)
	}
	if msg := strings.TrimSpace(stripControls(stderr.String())); msg != "" {
		return errors.New(msg)
	}
	return err
}

// capWriter keeps the first bytes written and drops the rest. A hook's stderr
// is held in memory to be shown as a reason, so a hook that streams to it must
// not be able to grow the process instead. The lock is not decoration:
// WaitDelay lets Wait return while the copying goroutine is still writing.
type capWriter struct {
	mu    sync.Mutex
	buf   bytes.Buffer
	limit int
}

func (w *capWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if room := w.limit - w.buf.Len(); room > 0 {
		if len(p) < room {
			room = len(p)
		}
		w.buf.Write(p[:room])
	}
	return len(p), nil
}

func (w *capWriter) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.String()
}
