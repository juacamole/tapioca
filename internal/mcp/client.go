// Package mcp implements a minimal Model Context Protocol client (JSON-RPC
// 2.0) over stdio and streamable HTTP.
package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"tapioca/internal/config"
	"tapioca/internal/secretenv"
)

// Protocol revisions, newest first. The client offers the first one and the
// server answers with the revision it will actually use, which is often an
// older one — most servers in the wild still speak 2024-11-05, and dropping
// them to chase the newest revision would be a regression, not an upgrade.
var protocolVersions = []string{"2025-06-18", "2025-03-26", "2024-11-05"}

const protocolVersion = "2025-06-18"

func supportedVersion(v string) bool {
	for _, known := range protocolVersions {
		if v == known {
			return true
		}
	}
	return false
}

// Tool is one tool exposed by a server.
type Tool struct {
	Server      string
	Name        string
	Description string
	InputSchema json.RawMessage
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type rpcMsg struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

// transport carries JSON-RPC messages to a server. Responses are handed back
// through Client.handle, whether they arrive on a pipe or an HTTP response.
type transport interface {
	Send(ctx context.Context, data []byte) error
	Close()
}

// Client is a connected MCP server, over stdio or HTTP.
type Client struct {
	Name string

	tr transport

	// Tools can be replaced while the agent is running, when the server sends
	// notifications/tools/list_changed.
	toolsMu   sync.Mutex
	tools     []Tool
	prompts   []Prompt
	resources []Resource
	version   string // the revision the handshake settled on
	onTools   func()

	pendingMu sync.Mutex
	pending   map[int64]chan *rpcMsg
	nextID    int64

	closeOnce sync.Once
	closed    chan struct{}
}

// Start connects to a server — an HTTP endpoint when url is set, otherwise a
// child process over stdio — then performs the handshake and lists its tools.
func Start(ctx context.Context, cfg config.MCPServerConfig) (*Client, error) {
	c := &Client{
		Name:    cfg.Name,
		pending: map[int64]chan *rpcMsg{},
		closed:  make(chan struct{}),
	}
	var err error
	if strings.TrimSpace(cfg.URL) != "" {
		err = c.startHTTP(cfg)
	} else {
		err = c.startStdio(cfg)
	}
	if err != nil {
		return nil, err
	}
	if err := c.handshake(ctx, cfg.Name); err != nil {
		c.Close()
		return nil, err
	}
	return c, nil
}

// handshake performs initialize, settles on a protocol revision and caches the
// server's tool list.
func (c *Client) handshake(ctx context.Context, name string) error {
	initCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	initParams := map[string]any{
		"protocolVersion": protocolVersion,
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "tapioca", "version": "0.1.0"},
	}
	res, err := c.call(initCtx, "initialize", initParams)
	if err != nil {
		return fmt.Errorf("mcp %s: initialize: %w", name, err)
	}
	var initRes struct {
		ProtocolVersion string     `json:"protocolVersion"`
		Capabilities    serverCaps `json:"capabilities"`
	}
	// A server that answers with nothing is taken at its word that it accepted
	// what we offered; one that names a revision decides which is used.
	agreed := protocolVersion
	if json.Unmarshal(res, &initRes) == nil && initRes.ProtocolVersion != "" {
		agreed = initRes.ProtocolVersion
	}
	if !supportedVersion(agreed) {
		return fmt.Errorf("mcp %s: server speaks protocol %s, this client speaks %s",
			name, agreed, strings.Join(protocolVersions, ", "))
	}
	c.toolsMu.Lock()
	c.version = agreed
	c.toolsMu.Unlock()
	// From 2025-06-18 every later HTTP request has to carry the revision.
	if h, ok := c.tr.(*httpTransport); ok {
		h.setVersion(agreed)
	}
	if err := c.notify(initCtx, "notifications/initialized", map[string]any{}); err != nil {
		return fmt.Errorf("mcp %s: %w", name, err)
	}
	if err := c.listTools(initCtx); err != nil {
		return fmt.Errorf("mcp %s: %w", name, err)
	}
	// Prompts and resources are optional, and a server offering neither answers
	// "method not found" rather than an empty list — so they are asked for only
	// when the server said it had them, and a refusal is not fatal.
	if initRes.Capabilities.Prompts != nil {
		_ = c.listPrompts(initCtx)
	}
	if initRes.Capabilities.Resources != nil {
		_ = c.listResources(initCtx)
	}
	return nil
}

// listTools fetches the server's tools, replacing whatever was cached.
func (c *Client) listTools(ctx context.Context) error {
	res, err := c.call(ctx, "tools/list", map[string]any{})
	if err != nil {
		return fmt.Errorf("tools/list: %w", err)
	}
	var list struct {
		Tools []struct {
			Name        string          `json:"name"`
			Description string          `json:"description"`
			InputSchema json.RawMessage `json:"inputSchema"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(res, &list); err != nil {
		return fmt.Errorf("decoding tools: %w", err)
	}
	tools := make([]Tool, 0, len(list.Tools))
	for _, t := range list.Tools {
		tools = append(tools, Tool{Server: c.Name, Name: t.Name, Description: t.Description, InputSchema: t.InputSchema})
	}
	c.toolsMu.Lock()
	c.tools = tools
	c.toolsMu.Unlock()
	return nil
}

// Tools returns the server's current tool list.
func (c *Client) Tools() []Tool {
	c.toolsMu.Lock()
	defer c.toolsMu.Unlock()
	return append([]Tool(nil), c.tools...)
}

// Version returns the protocol revision the handshake settled on.
func (c *Client) Version() string {
	c.toolsMu.Lock()
	defer c.toolsMu.Unlock()
	return c.version
}

// OnToolsChanged registers a callback for when the server's tool list changes,
// so the UI can redraw its MCP panel without polling.
func (c *Client) OnToolsChanged(fn func()) {
	c.toolsMu.Lock()
	c.onTools = fn
	c.toolsMu.Unlock()
}

// refreshTools re-lists after a tools/list_changed notification. It runs on
// the read loop's goroutine, so it cannot block waiting for its own response.
func (c *Client) refreshTools() { c.refreshList(c.listTools) }

func (c *Client) startStdio(cfg config.MCPServerConfig) error {
	cmd := exec.Command(cfg.Command, cfg.Args...)
	// Provider credentials stay with Tapioca; servers that need secrets get
	// them explicitly through their own [mcp.env] block.
	cmd.Env = secretenv.Scrubbed()
	for k, v := range cfg.Env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	cmd.Stderr = io.Discard
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("mcp %s: starting %s: %w", cfg.Name, cfg.Command, err)
	}
	c.tr = &stdioTransport{cmd: cmd, stdin: stdin}

	// Wait must not run until the read loop has drained the pipe, or a
	// server that replies and immediately exits loses its final response.
	readDone := make(chan struct{})
	go func() {
		c.readLoop(stdout)
		close(readDone)
	}()
	go func() {
		<-readDone
		_ = cmd.Wait()
		c.Close()
	}()
	return nil
}

// stdioTransport speaks newline-delimited JSON-RPC to a child process.
type stdioTransport struct {
	cmd     *exec.Cmd
	stdin   io.WriteCloser
	writeMu sync.Mutex
}

func (s *stdioTransport) Send(_ context.Context, data []byte) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	_, err := s.stdin.Write(append(data, '\n'))
	return err
}

func (s *stdioTransport) Close() {
	_ = s.stdin.Close()
	if s.cmd.Process != nil {
		_ = s.cmd.Process.Kill()
	}
}

func (c *Client) readLoop(stdout io.Reader) {
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		c.handle(line)
	}
	c.Close()
}

// handle routes one incoming JSON-RPC message, from either transport.
func (c *Client) handle(line []byte) {
	var msg rpcMsg
	if err := json.Unmarshal(line, &msg); err != nil {
		return
	}
	switch {
	case msg.Method != "" && msg.ID != nil:
		// Server-initiated request: answer ping, reject the rest.
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if msg.Method == "ping" {
			_ = c.send(ctx, rpcMsg{JSONRPC: "2.0", ID: msg.ID, Result: json.RawMessage("{}")})
		} else {
			_ = c.send(ctx, rpcMsg{JSONRPC: "2.0", ID: msg.ID, Error: &rpcError{Code: -32601, Message: "method not supported"}})
		}
	case msg.Method != "":
		// A server whose tools change mid-session (a gateway that connects to
		// another backend, say) would otherwise keep offering the old list
		// until restart.
		switch msg.Method {
		case "notifications/tools/list_changed":
			c.refreshTools()
		case "notifications/prompts/list_changed":
			c.refreshPrompts()
		case "notifications/resources/list_changed":
			c.refreshResources()
		}
	default:
		var id int64
		if json.Unmarshal(msg.ID, &id) != nil {
			return
		}
		c.pendingMu.Lock()
		ch := c.pending[id]
		delete(c.pending, id)
		c.pendingMu.Unlock()
		if ch != nil {
			m := msg
			ch <- &m
		}
	}
}

func (c *Client) send(ctx context.Context, msg rpcMsg) error {
	data, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	return c.tr.Send(ctx, data)
}

func (c *Client) notify(ctx context.Context, method string, params any) error {
	p, err := json.Marshal(params)
	if err != nil {
		return err
	}
	return c.send(ctx, rpcMsg{JSONRPC: "2.0", Method: method, Params: p})
}

func (c *Client) call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	p, err := json.Marshal(params)
	if err != nil {
		return nil, err
	}
	c.pendingMu.Lock()
	c.nextID++
	id := c.nextID
	ch := make(chan *rpcMsg, 1)
	c.pending[id] = ch
	c.pendingMu.Unlock()
	defer func() {
		c.pendingMu.Lock()
		delete(c.pending, id)
		c.pendingMu.Unlock()
	}()

	req := rpcMsg{JSONRPC: "2.0", ID: json.RawMessage(strconv.FormatInt(id, 10)), Method: method, Params: p}
	if err := c.send(ctx, req); err != nil {
		return nil, err
	}
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-c.closed:
		return nil, fmt.Errorf("server %s exited", c.Name)
	case resp := <-ch:
		if resp.Error != nil {
			return nil, fmt.Errorf("%s (code %d)", resp.Error.Message, resp.Error.Code)
		}
		return resp.Result, nil
	}
}

// CallTool invokes a tool and returns its text output.
func (c *Client) CallTool(ctx context.Context, name string, args json.RawMessage) (string, bool, error) {
	if len(args) == 0 {
		args = json.RawMessage("{}")
	}
	res, err := c.call(ctx, "tools/call", map[string]any{"name": name, "arguments": args})
	if err != nil {
		return "", false, err
	}
	var out struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		IsError bool `json:"isError"`
	}
	if err := json.Unmarshal(res, &out); err != nil {
		return string(res), false, nil
	}
	var parts []string
	for _, item := range out.Content {
		if item.Type == "text" {
			parts = append(parts, item.Text)
		} else {
			parts = append(parts, fmt.Sprintf("[%s content omitted]", item.Type))
		}
	}
	text := ""
	for i, p := range parts {
		if i > 0 {
			text += "\n"
		}
		text += p
	}
	return text, out.IsError, nil
}

// Alive reports whether the connection is still usable.
func (c *Client) Alive() bool {
	select {
	case <-c.closed:
		return false
	default:
		return true
	}
}

// Close disconnects: kills the child process, or drops the HTTP session.
func (c *Client) Close() {
	c.closeOnce.Do(func() {
		close(c.closed)
		if c.tr != nil {
			c.tr.Close()
		}
	})
}
