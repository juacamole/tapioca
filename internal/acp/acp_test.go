package acp

import (
	"bufio"
	"encoding/json"
	"io"
	"strings"
	"sync"
	"testing"
	"time"
)

// client drives the server the way an editor would: writes requests, reads
// notifications and replies off one pipe.
type client struct {
	t   *testing.T
	in  io.WriteCloser // to the server
	out *bufio.Scanner // from the server
	id  int
}

func newTestClient(t *testing.T, serve func(in io.Reader, out io.Writer)) *client {
	t.Helper()
	cin, sin := io.Pipe()
	sout, cout := io.Pipe()
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		serve(cin, cout)
		cout.Close()
	}()
	t.Cleanup(func() {
		sin.Close()
		wg.Wait()
	})
	sc := bufio.NewScanner(sout)
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	return &client{t: t, in: sin, out: sc}
}

func (c *client) send(method string, params any) int {
	c.t.Helper()
	c.id++
	msg := map[string]any{"jsonrpc": "2.0", "id": c.id, "method": method}
	if params != nil {
		msg["params"] = params
	}
	data, err := json.Marshal(msg)
	if err != nil {
		c.t.Fatal(err)
	}
	if _, err := c.in.Write(append(data, '\n')); err != nil {
		c.t.Fatal(err)
	}
	return c.id
}

// await reads until the reply to id arrives, collecting notifications.
func (c *client) await(id int) (json.RawMessage, *rpcError, []rpcMsg) {
	c.t.Helper()
	var notes []rpcMsg
	deadline := time.Now().Add(15 * time.Second)
	for c.out.Scan() {
		if time.Now().After(deadline) {
			c.t.Fatal("timed out waiting for a reply")
		}
		var msg rpcMsg
		if err := json.Unmarshal(c.out.Bytes(), &msg); err != nil {
			continue
		}
		if msg.Method != "" {
			notes = append(notes, msg)
			continue
		}
		var got int
		if json.Unmarshal(msg.ID, &got) == nil && got == id {
			return msg.Result, msg.Error, notes
		}
	}
	c.t.Fatalf("connection closed before the reply to %d", id)
	return nil, nil, notes
}

func TestInitializeAndSessionLifecycle(t *testing.T) {
	c := newTestClient(t, func(in io.Reader, out io.Writer) {
		_ = Serve(testConfig(t), Launch{}, in, out)
	})

	res, rpcErr, _ := c.await(c.send("initialize", map[string]any{
		"protocolVersion":    1,
		"clientCapabilities": map[string]any{},
		"clientInfo":         map[string]any{"name": "test-editor", "version": "1"},
	}))
	if rpcErr != nil {
		t.Fatalf("initialize failed: %+v", rpcErr)
	}
	var init struct {
		ProtocolVersion int `json:"protocolVersion"`
		AgentInfo       struct {
			Name string `json:"name"`
		} `json:"agentInfo"`
		AuthMethods []any `json:"authMethods"`
	}
	if err := json.Unmarshal(res, &init); err != nil {
		t.Fatal(err)
	}
	if init.ProtocolVersion != protocolVersion {
		t.Errorf("protocolVersion = %d, want %d", init.ProtocolVersion, protocolVersion)
	}
	if init.AgentInfo.Name != "tapioca" {
		t.Errorf("agentInfo.name = %q", init.AgentInfo.Name)
	}
	if init.AuthMethods == nil {
		t.Error("authMethods must be present (empty means no auth needed), not null")
	}

	// session/new with a real directory.
	res, rpcErr, _ = c.await(c.send("session/new", map[string]any{
		"cwd":        t.TempDir(),
		"mcpServers": []any{},
	}))
	if rpcErr != nil {
		t.Fatalf("session/new failed: %+v", rpcErr)
	}
	var sess struct {
		SessionID string `json:"sessionId"`
	}
	if err := json.Unmarshal(res, &sess); err != nil || sess.SessionID == "" {
		t.Fatalf("no sessionId returned: %s", res)
	}

	// Unknown sessions and bad directories are rejected, not panicked on.
	_, rpcErr, _ = c.await(c.send("session/prompt", map[string]any{
		"sessionId": "nope",
		"prompt":    []any{map[string]any{"type": "text", "text": "hi"}},
	}))
	if rpcErr == nil {
		t.Error("prompting an unknown session should fail")
	}
	_, rpcErr, _ = c.await(c.send("session/new", map[string]any{"cwd": "/definitely/not/here"}))
	if rpcErr == nil {
		t.Error("session/new with a bad cwd should fail")
	}
	_, rpcErr, _ = c.await(c.send("session/unsupported", map[string]any{}))
	if rpcErr == nil || rpcErr.Code != -32601 {
		t.Errorf("unknown method should return -32601, got %+v", rpcErr)
	}
}

func TestPromptStreamsUpdatesAndStops(t *testing.T) {
	cfg := testConfig(t)
	stub := stubProvider(t, cfg)

	c := newTestClient(t, func(in io.Reader, out io.Writer) {
		_ = Serve(cfg, Launch{}, in, out)
	})
	c.await(c.send("initialize", map[string]any{"protocolVersion": 1}))
	res, _, _ := c.await(c.send("session/new", map[string]any{"cwd": t.TempDir()}))
	var sess struct {
		SessionID string `json:"sessionId"`
	}
	json.Unmarshal(res, &sess)

	res, rpcErr, notes := c.await(c.send("session/prompt", map[string]any{
		"sessionId": sess.SessionID,
		"prompt": []any{
			map[string]any{"type": "text", "text": "hello"},
			map[string]any{"type": "resource", "resource": map[string]any{
				"uri": "file:///tmp/x.go", "text": "package x",
			}},
		},
	}))
	if rpcErr != nil {
		t.Fatalf("session/prompt failed: %+v", rpcErr)
	}
	var out struct {
		StopReason string `json:"stopReason"`
	}
	if err := json.Unmarshal(res, &out); err != nil || out.StopReason != "end_turn" {
		t.Fatalf("stopReason = %s (%v)", res, err)
	}

	// The prompt must reach the model with both blocks flattened in.
	got := stub.lastPrompt()
	if !strings.Contains(got, "hello") || !strings.Contains(got, "package x") {
		t.Errorf("content blocks lost on the way to the model: %q", got)
	}

	var chunks []string
	for _, n := range notes {
		if n.Method != "session/update" {
			continue
		}
		var p struct {
			SessionID string `json:"sessionId"`
			Update    struct {
				SessionUpdate string `json:"sessionUpdate"`
				Content       struct {
					Text string `json:"text"`
				} `json:"content"`
			} `json:"update"`
		}
		if json.Unmarshal(n.Params, &p) != nil {
			continue
		}
		if p.SessionID != sess.SessionID {
			t.Errorf("update tagged with the wrong session: %q", p.SessionID)
		}
		if p.Update.SessionUpdate == "agent_message_chunk" {
			chunks = append(chunks, p.Update.Content.Text)
		}
	}
	if strings.Join(chunks, "") != "hi there" {
		t.Errorf("streamed text = %q, want %q", strings.Join(chunks, ""), "hi there")
	}
}

// Tool calls are the part editors render specially, so the bridge is checked
// against a model that actually asks for one rather than hoping a live model does.
func TestToolCallsBecomeUpdates(t *testing.T) {
	cfg := testConfig(t)
	work := t.TempDir()
	stub := stubProvider(t, cfg)
	stub.toolCall = "glob"
	stub.toolArgs = `{"pattern":"**/*.go"}`

	c := newTestClient(t, func(in io.Reader, out io.Writer) {
		_ = Serve(cfg, Launch{}, in, out)
	})
	c.await(c.send("initialize", map[string]any{"protocolVersion": 1}))
	res, _, _ := c.await(c.send("session/new", map[string]any{"cwd": work}))
	var sess struct {
		SessionID string `json:"sessionId"`
	}
	json.Unmarshal(res, &sess)

	_, rpcErr, notes := c.await(c.send("session/prompt", map[string]any{
		"sessionId": sess.SessionID,
		"prompt":    []any{map[string]any{"type": "text", "text": "list go files"}},
	}))
	if rpcErr != nil {
		t.Fatalf("prompt failed: %+v", rpcErr)
	}

	var started, finished bool
	var startID, endID, kind, endStatus string
	for _, n := range notes {
		var p struct {
			Update struct {
				SessionUpdate string `json:"sessionUpdate"`
				ToolCallID    string `json:"toolCallId"`
				Kind          string `json:"kind"`
				Status        string `json:"status"`
				Title         string `json:"title"`
			} `json:"update"`
		}
		if n.Method != "session/update" || json.Unmarshal(n.Params, &p) != nil {
			continue
		}
		switch p.Update.SessionUpdate {
		case "tool_call":
			started, startID, kind = true, p.Update.ToolCallID, p.Update.Kind
			if !strings.Contains(p.Update.Title, "glob") {
				t.Errorf("tool_call title does not name the tool: %q", p.Update.Title)
			}
		case "tool_call_update":
			finished, endID, endStatus = true, p.Update.ToolCallID, p.Update.Status
		}
	}
	if !started || !finished {
		t.Fatalf("missing tool notifications (start=%v end=%v)", started, finished)
	}
	if startID != endID {
		t.Errorf("toolCallId changed between start (%q) and update (%q); editors key on it", startID, endID)
	}
	if kind != "search" {
		t.Errorf("glob mapped to kind %q, want search", kind)
	}
	if endStatus != "completed" {
		t.Errorf("final status = %q, want completed", endStatus)
	}
}

func TestPromptTextFlattensBlocks(t *testing.T) {
	blocks := []json.RawMessage{
		json.RawMessage(`{"type":"text","text":"first"}`),
		json.RawMessage(`{"type":"image","data":"ignored"}`),
		json.RawMessage(`{"type":"resource","resource":{"uri":"file:///a.go","text":"body"}}`),
		json.RawMessage(`{"type":"resource","resource":{"uri":"file:///b.go"}}`),
		json.RawMessage(`not json`),
	}
	got := promptText(blocks)
	for _, want := range []string{"first", "file:///a.go", "body", "file:///b.go"} {
		if !strings.Contains(got, want) {
			t.Errorf("promptText dropped %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "ignored") {
		t.Errorf("unsupported block leaked into the prompt:\n%s", got)
	}
}

func TestToolKindMapping(t *testing.T) {
	cases := map[string]string{
		"bash": "execute", "read_file": "read", "grep": "search",
		"write_file": "edit", "web_fetch": "fetch", "todo_write": "think",
		"something_else": "other",
	}
	for name, want := range cases {
		if got := toolKind(name); got != want {
			t.Errorf("toolKind(%q) = %q, want %q", name, got, want)
		}
	}
}

// A model that calls spawn_agent over ACP must get an answer: nothing here can
// run a subagent, and an unanswered request blocks the turn forever.
func TestSpawnRequestDoesNotHangTheTurn(t *testing.T) {
	cfg := testConfig(t)
	stub := stubProvider(t, cfg)
	stub.toolCall = "spawn_agent"
	stub.toolArgs = `{"task":"do something else","name":"helper"}`

	c := newTestClient(t, func(in io.Reader, out io.Writer) {
		_ = Serve(cfg, Launch{}, in, out)
	})
	c.await(c.send("initialize", map[string]any{"protocolVersion": 1}))
	res, _, _ := c.await(c.send("session/new", map[string]any{"cwd": t.TempDir()}))
	var sess struct {
		SessionID string `json:"sessionId"`
	}
	json.Unmarshal(res, &sess)

	done := make(chan string, 1)
	go func() {
		r, _, _ := c.await(c.send("session/prompt", map[string]any{
			"sessionId": sess.SessionID,
			"prompt":    []any{map[string]any{"type": "text", "text": "delegate this"}},
		}))
		done <- string(r)
	}()

	select {
	case got := <-done:
		if !strings.Contains(got, "end_turn") {
			t.Fatalf("turn ended oddly: %s", got)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("session/prompt hung on spawn_agent")
	}
}
