package acp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"tapioca/internal/agent"
	"tapioca/internal/config"
	"tapioca/internal/provider"
	"tapioca/internal/tools"
	"tapioca/internal/version"
)

// server.go makes Tapioca the agent an editor drives. This is the other
// direction: Tapioca launches another agent and drives it, so a tab in the TUI
// is a frontend for whatever speaks the protocol. The framing, the message
// types and the tool vocabulary are shared with the server — one connection
// implementation, pointed either way.

// cancelGrace bounds how long a cancelled turn waits for the other side to
// acknowledge before the tab is handed back to the user. An agent that ignores
// session/cancel must not leave the tab wedged.
const cancelGrace = 10 * time.Second

// stderrTail is how much of a crashing agent's error output is kept. Enough
// for a stack trace's first frames or a missing-credential message, which is
// what makes a crash report worth reading.
const stderrTail = 4 << 10

// Client drives one external agent over stdio.
type Client struct {
	Name string

	conn *conn
	gate *tools.Executor

	ctx  context.Context // client lifetime; done once the connection is gone
	stop context.CancelFunc

	stdin     io.WriteCloser
	proc      *exec.Cmd
	sessionID string
	info      string // what the agent called itself in the handshake

	closeOnce sync.Once

	mu      sync.Mutex
	agent   *agent.Agent
	turn    *turn
	exit    string // why the connection ended, once it has
	closing bool   // Close was called; an exit is expected, not news
}

// Dial launches the configured agent and connects to it. cwd is the session's
// working directory: the external agent works on the same tree Tapioca does,
// which is the point of running it from here.
func Dial(ctx context.Context, cfg config.ExternalAgent, cwd string, gate *tools.Executor) (*Client, error) {
	if strings.TrimSpace(cfg.Command) == "" {
		return nil, fmt.Errorf("external agent %q has no command", cfg.Name)
	}
	cmd := exec.Command(cfg.Command, cfg.Args...)
	cmd.Dir = cwd
	// The whole environment, unlike an MCP server's: an agent that cannot
	// reach a model is not an agent, and the credentials Tapioca scrubs from
	// tool subprocesses are exactly the ones this one needs. Withholding them
	// would only move the same keys into config.toml in plain text.
	cmd.Env = os.Environ()
	for k, v := range cfg.Env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	errs := &tailWriter{limit: stderrTail}
	cmd.Stderr = errs
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("starting %s: %w", cfg.Command, err)
	}

	c := newClient(cfg.Name, stdin, gate)
	c.proc = cmd
	c.watch(stdout, func() string {
		// Wait only after the read loop has drained stdout, or an agent that
		// answers and exits immediately loses its last message.
		err := cmd.Wait()
		return exitReason(cfg.Name, err, errs.String())
	})
	if err := c.handshake(ctx, cwd); err != nil {
		c.Close()
		return nil, err
	}
	return c, nil
}

// Connect speaks the protocol to an agent already running behind in and out.
// Dial is this plus the process around it.
func Connect(ctx context.Context, name string, in io.Reader, out io.WriteCloser, cwd string, gate *tools.Executor) (*Client, error) {
	c := newClient(name, out, gate)
	c.watch(in, func() string { return name + " closed the connection" })
	if err := c.handshake(ctx, cwd); err != nil {
		c.Close()
		return nil, err
	}
	return c, nil
}

func newClient(name string, out io.WriteCloser, gate *tools.Executor) *Client {
	ctx, stop := context.WithCancel(context.Background())
	return &Client{Name: name, conn: newConn(out), gate: gate, ctx: ctx, stop: stop, stdin: out}
}

// watch runs the read loop and records why it ended.
func (c *Client) watch(in io.Reader, reason func() string) {
	go func() {
		c.readLoop(in)
		c.disconnected(reason())
	}()
}

// Attach points the client at the tab it reports into. Kept out of Dial so a
// connection that never comes up leaves no agent behind.
func (c *Client) Attach(a *agent.Agent) {
	c.mu.Lock()
	c.agent = a
	c.mu.Unlock()
}

func (c *Client) tab() *agent.Agent {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.agent
}

// emit reports to the tab, if there is one yet. Events raised between Dial and
// Attach have nowhere to go and are dropped rather than held: the only ones
// that can arrive in that window announce a connection the caller is about to
// be told about anyway.
func (c *Client) emit(ev agent.Event) {
	if a := c.tab(); a != nil {
		a.Emit(ev)
	}
}

// Close disconnects and stops the process. Safe to call twice: the frontend
// closes a tab, and a session teardown closes everything again.
func (c *Client) Close() {
	c.closeOnce.Do(func() {
		c.mu.Lock()
		c.closing = true
		c.mu.Unlock()
		c.stop()
		_ = c.stdin.Close()
		if c.proc != nil && c.proc.Process != nil {
			_ = c.proc.Process.Kill()
		}
	})
}

// disconnected releases everything waiting on the connection and reports the
// loss, once. A crash out there is news for the tab and nothing more — the
// session, the other agents and the transcript survive it.
func (c *Client) disconnected(reason string) {
	c.mu.Lock()
	first := c.exit == ""
	if first {
		c.exit = reason
	}
	quiet := c.closing
	c.mu.Unlock()
	c.stop() // wakes the pending prompt and any permission prompt in front of the user
	if first && !quiet {
		c.emit(agent.Event{Kind: agent.EvNotice, Text: reason})
	}
}

// exitError describes a connection that is already gone, for a call that
// cannot be answered any more.
func (c *Client) exitError() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.exit != "" {
		return errors.New(c.exit)
	}
	return errors.New(c.Name + " is not connected")
}

func (c *Client) readLoop(in io.Reader) {
	sc := bufio.NewScanner(in)
	sc.Buffer(make([]byte, 0, 64*1024), 32*1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		if len(strings.TrimSpace(string(line))) == 0 {
			continue
		}
		var msg rpcMsg
		if err := json.Unmarshal(line, &msg); err != nil {
			continue
		}
		if msg.Method == "" { // a reply to something we asked
			m := msg
			c.conn.routeResponse(&m)
			continue
		}
		c.dispatch(&msg)
	}
}

// dispatch handles one message from the agent. Notifications are handled on
// this goroutine so the turn's events reach the frontend in the order they
// happened; only a permission request steps aside, because it blocks on a
// human and the connection has to keep reading while it does.
func (c *Client) dispatch(msg *rpcMsg) {
	switch msg.Method {
	case "session/update":
		c.handleUpdate(msg.Params)
	case "session/request_permission":
		// The prompt belongs to the turn that provoked it: the turn counts it
		// so it cannot end first, and the turn's cancellation releases it.
		// One that arrives outside a turn is still gated, just unattached.
		c.mu.Lock()
		t, ctx := c.turn, c.ctx
		if t != nil {
			t.perms.Add(1)
			ctx = t.ctx
		}
		c.mu.Unlock()
		id, params := msg.ID, msg.Params
		go func() {
			if t != nil {
				defer t.perms.Done()
			}
			c.handlePermission(ctx, id, params)
		}()
	default:
		// fs/* and terminal/* are the methods an agent uses to make the client
		// act for it. Tapioca declines them in initialize: work it did on
		// another agent's behalf would be work its own gate never saw.
		if msg.ID != nil {
			c.conn.fail(msg.ID, -32601, "method not supported: "+msg.Method)
		}
	}
}

// call binds a request to the life of the connection, so one waiting on an
// agent that has already died fails with the reason it died rather than at the
// caller's timeout.
func (c *Client) call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	defer context.AfterFunc(c.ctx, cancel)()
	res, err := c.conn.call(ctx, method, params)
	if err != nil && c.ctx.Err() != nil {
		return nil, c.exitError()
	}
	return res, err
}

func (c *Client) handshake(ctx context.Context, cwd string) error {
	res, err := c.call(ctx, "initialize", map[string]any{
		"protocolVersion": protocolVersion,
		"clientCapabilities": map[string]any{
			// Tapioca reads and writes files with its own tools, under its own
			// gate. Lending them to the agent would be a second way into the
			// working tree, and one the permission rules never see.
			"fs":       map[string]any{"readTextFile": false, "writeTextFile": false},
			"terminal": false,
		},
		"clientInfo": map[string]any{"name": "tapioca", "version": version.Version},
	})
	if err != nil {
		return fmt.Errorf("%s: initialize: %w", c.Name, err)
	}
	var init struct {
		ProtocolVersion int `json:"protocolVersion"`
		AgentInfo       struct {
			Name    string `json:"name"`
			Version string `json:"version"`
		} `json:"agentInfo"`
	}
	// A version of its own is the agent's answer to ours; silence means it
	// took what we offered.
	if json.Unmarshal(res, &init) == nil {
		if init.ProtocolVersion > protocolVersion {
			return fmt.Errorf("%s speaks ACP %d, this client speaks %d", c.Name, init.ProtocolVersion, protocolVersion)
		}
		// What the agent calls itself goes where a message's model normally
		// goes: the transcript's job is to say who answered, and for this tab
		// that is a program rather than a model.
		c.info = strings.TrimSpace(init.AgentInfo.Name + " " + init.AgentInfo.Version)
	}
	res, err = c.call(ctx, "session/new", map[string]any{
		"cwd": cwd,
		// Tapioca's MCP servers stay Tapioca's. Handing the list over would
		// give the external agent tools nobody chose for it.
		"mcpServers": []any{},
	})
	if err != nil {
		return fmt.Errorf("%s: session/new: %w", c.Name, err)
	}
	var s struct {
		SessionID string `json:"sessionId"`
	}
	if json.Unmarshal(res, &s) != nil || s.SessionID == "" {
		return fmt.Errorf("%s: session/new returned no session id", c.Name)
	}
	c.sessionID = s.SessionID
	return nil
}

// Prompt runs one turn on the external agent and returns when it has ended.
func (c *Client) Prompt(ctx context.Context, text string) error {
	if strings.TrimSpace(text) == "" {
		return errors.New("empty prompt")
	}
	if c.ctx.Err() != nil {
		return c.exitError()
	}
	c.mu.Lock()
	c.turn = &turn{ctx: ctx, model: c.info, calls: map[string]int{}}
	c.mu.Unlock()

	callCtx, cancelCall := context.WithCancel(c.ctx)
	defer cancelCall()
	type reply struct {
		res json.RawMessage
		err error
	}
	done := make(chan reply, 1)
	go func() {
		res, err := c.conn.call(callCtx, "session/prompt", map[string]any{
			"sessionId": c.sessionID,
			"prompt":    []any{textBlock(text)},
		})
		done <- reply{res, err}
	}()

	var r reply
	select {
	case r = <-done:
	case <-ctx.Done():
		// The turn is the agent's to stop; asking is the protocol's way and
		// leaves its history consistent. Waiting forever for an agent that
		// ignores the request is not, hence the grace.
		_ = c.conn.notify("session/cancel", map[string]any{"sessionId": c.sessionID})
		select {
		case r = <-done:
		case <-time.After(cancelGrace):
			r = reply{err: errors.New(c.Name + " did not acknowledge the cancellation")}
		}
	}
	// Nothing may follow the end of the turn: an approval the user has not
	// given yet is still part of it. Detaching the turn first means no further
	// prompt can join the one being waited for.
	if t := c.takeTurn(); t != nil {
		t.perms.Wait()
		for _, m := range t.flush() {
			msg := m
			c.emit(agent.Event{Kind: agent.EvMessage, Message: &msg})
		}
	}

	if r.err != nil {
		if c.ctx.Err() != nil {
			return c.exitError()
		}
		if errors.Is(r.err, context.Canceled) {
			return nil // the user stopped it; that is not a failure
		}
		return r.err
	}
	var out struct {
		StopReason string `json:"stopReason"`
	}
	if json.Unmarshal(r.res, &out) == nil {
		switch out.StopReason {
		case "refusal":
			return errors.New(c.Name + " refused the request")
		case "max_turn_requests", "max_tokens":
			c.emit(agent.Event{Kind: agent.EvNotice, Text: c.Name + " stopped early: " + out.StopReason})
		}
	}
	return nil
}

// turn is what the agent has reported since the last flush. The read loop
// writes it, Prompt reads it, and neither touches it without the client's lock
// — except after takeTurn, which leaves nobody else holding a reference.
type turn struct {
	ctx     context.Context
	model   string           // what the agent calls itself, for the transcript
	perms   sync.WaitGroup   // permission prompts this turn is waiting on
	blocks  []provider.Block // this round's assistant content, in arrival order
	results []provider.Block // the tool results answering it
	calls   map[string]int   // toolCallId -> index in blocks
}

// takeTurn detaches the turn in flight. What arrives after it belongs to a turn
// that has ended, and is dropped rather than added to the next one.
func (c *Client) takeTurn() *turn {
	c.mu.Lock()
	defer c.mu.Unlock()
	t := c.turn
	c.turn = nil
	return t
}

// flush turns a completed round into the messages a local turn would have
// produced: one assistant message carrying the text and the calls it made,
// followed by the results answering them. The transcript renders external
// work exactly as it renders Tapioca's own.
func (t *turn) flush() []provider.Message {
	var out []provider.Message
	if len(t.blocks) > 0 {
		out = append(out, provider.Message{Role: "assistant", Blocks: t.blocks, Model: t.model, Time: time.Now()})
	}
	if len(t.results) > 0 {
		out = append(out, provider.Message{Role: "user", Blocks: t.results, Time: time.Now()})
	}
	t.blocks, t.results = nil, nil
	t.calls = map[string]int{}
	return out
}

// appendText adds a delta to the trailing block of its kind, so a streamed
// paragraph stays one block instead of one block per chunk.
func (t *turn) appendText(kind, text string) {
	if n := len(t.blocks); n > 0 && t.blocks[n-1].Type == kind {
		t.blocks[n-1].Text += text
		return
	}
	t.blocks = append(t.blocks, provider.Block{Type: kind, Text: text})
}

type contentBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

func (c *Client) handleUpdate(params json.RawMessage) {
	var u struct {
		Update json.RawMessage `json:"update"`
	}
	if json.Unmarshal(params, &u) != nil || len(u.Update) == 0 {
		return
	}
	var head struct {
		Kind string `json:"sessionUpdate"`
	}
	if json.Unmarshal(u.Update, &head) != nil {
		return
	}
	switch head.Kind {
	case "agent_message_chunk":
		c.chunk(u.Update, "text", agent.EvTextDelta)
	case "agent_thought_chunk":
		c.chunk(u.Update, "thinking", agent.EvThinkingDelta)
	case "tool_call":
		c.toolCall(u.Update)
	case "tool_call_update":
		c.toolUpdate(u.Update)
	case "plan":
		c.plan(u.Update)
	}
}

func (c *Client) chunk(raw json.RawMessage, kind string, ev agent.EventKind) {
	var u struct {
		Content contentBlock `json:"content"`
	}
	if json.Unmarshal(raw, &u) != nil || u.Content.Text == "" {
		return
	}
	c.mu.Lock()
	if c.turn != nil {
		c.turn.appendText(kind, u.Content.Text)
	}
	c.mu.Unlock()
	c.emit(agent.Event{Kind: ev, Text: u.Content.Text})
}

func (c *Client) toolCall(raw json.RawMessage) {
	var u toolCallUpdate
	if json.Unmarshal(raw, &u) != nil {
		return
	}
	name := toolNameFor(u.Kind)
	input := u.Raw
	if len(input) == 0 {
		input = json.RawMessage(quoteJSON(u.Title))
	}
	var msgs []provider.Message
	c.mu.Lock()
	if t := c.turn; t != nil {
		// Results already collected mean the previous round is over: flush it
		// so the transcript reads call, answer, call, answer.
		if len(t.results) > 0 {
			msgs = t.flush()
		}
		t.calls[u.ID] = len(t.blocks)
		t.blocks = append(t.blocks, provider.Block{
			Type: "tool_use", ID: u.ID, Name: name, Input: input,
		})
	}
	c.mu.Unlock()
	for i := range msgs {
		m := msgs[i]
		c.emit(agent.Event{Kind: agent.EvMessage, Message: &m})
	}
	c.emit(agent.Event{Kind: agent.EvToolStart, Tool: &agent.ToolInfo{Name: name, Args: u.Title}})
}

func (c *Client) toolUpdate(raw json.RawMessage) {
	var u toolCallUpdate
	if json.Unmarshal(raw, &u) != nil || u.ID == "" {
		return
	}
	switch u.Status {
	case "completed", "failed":
	default:
		return // in_progress carries no outcome worth recording
	}
	text := u.resultText()
	isErr := u.Status == "failed"
	name := toolNameFor(u.Kind)
	c.mu.Lock()
	if t := c.turn; t != nil {
		if i, ok := t.calls[u.ID]; ok {
			name = t.blocks[i].Name
		}
		t.results = append(t.results, provider.Block{
			Type: "tool_result", ToolUseID: u.ID, Name: name, Content: text, IsError: isErr,
		})
	}
	c.mu.Unlock()
	c.emit(agent.Event{Kind: agent.EvToolEnd, Tool: &agent.ToolInfo{
		Name: name, Args: u.Title, Result: text, IsError: isErr,
	}})
}

// plan mirrors the agent's own plan into the todo panel, which is where a
// reader already looks for what an agent intends to do next.
func (c *Client) plan(raw json.RawMessage) {
	var u struct {
		Entries []struct {
			Content string `json:"content"`
			Status  string `json:"status"`
		} `json:"entries"`
	}
	if json.Unmarshal(raw, &u) != nil {
		return
	}
	a := c.tab()
	if a == nil {
		return
	}
	items := make([]agent.TodoItem, 0, len(u.Entries))
	for _, e := range u.Entries {
		if strings.TrimSpace(e.Content) == "" {
			continue
		}
		items = append(items, agent.TodoItem{Content: e.Content, Status: e.Status})
	}
	a.SetTodos(items)
}

// toolCallUpdate is a tool_call or tool_call_update notification.
type toolCallUpdate struct {
	ID      string          `json:"toolCallId"`
	Title   string          `json:"title"`
	Kind    string          `json:"kind"`
	Status  string          `json:"status"`
	Raw     json.RawMessage `json:"rawInput"`
	Content []struct {
		Type    string       `json:"type"`
		Content contentBlock `json:"content"`
	} `json:"content"`
	Locations []struct {
		Path string `json:"path"`
	} `json:"locations"`
}

// resultText flattens whatever the agent attached to a finished call.
func (u toolCallUpdate) resultText() string {
	var parts []string
	for _, c := range u.Content {
		if t := strings.TrimSpace(c.Content.Text); t != "" {
			parts = append(parts, t)
		}
	}
	return strings.Join(parts, "\n")
}

func quoteJSON(s string) string {
	b, err := json.Marshal(s)
	if err != nil {
		return `""`
	}
	return string(b)
}

// tailWriter keeps the last bytes written to it. A crashing agent's useful
// output is at the end — the error it died on, not the banner it started with.
type tailWriter struct {
	limit int
	mu    sync.Mutex
	buf   []byte
}

func (t *tailWriter) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = append(t.buf, p...)
	if len(t.buf) > t.limit {
		t.buf = t.buf[len(t.buf)-t.limit:]
	}
	return len(p), nil
}

func (t *tailWriter) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return string(t.buf)
}

// exitReason says what happened to the process in the terms a user can act on:
// which agent, how it ended, and whatever it last complained about.
func exitReason(name string, err error, stderr string) string {
	msg := name + " exited"
	if err != nil {
		msg += ": " + err.Error()
	}
	if s := strings.TrimSpace(stderr); s != "" {
		if len(s) > 300 {
			s = s[len(s)-300:]
		}
		msg += " — " + strings.ReplaceAll(s, "\n", " ")
	}
	return msg
}
