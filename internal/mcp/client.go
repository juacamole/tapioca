// Package mcp implements a minimal Model Context Protocol client over the
// stdio transport (newline-delimited JSON-RPC 2.0).
package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"sync"
	"time"

	"tapioca/internal/config"
)

const protocolVersion = "2024-11-05"

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

// Client is a connected MCP server process.
type Client struct {
	Name  string
	Tools []Tool

	cmd     *exec.Cmd
	stdin   io.WriteCloser
	writeMu sync.Mutex

	pendingMu sync.Mutex
	pending   map[int64]chan *rpcMsg
	nextID    int64

	closeOnce sync.Once
	closed    chan struct{}
}

// Start launches the server process, performs the initialize handshake and
// lists its tools.
func Start(ctx context.Context, cfg config.MCPServerConfig) (*Client, error) {
	cmd := exec.Command(cfg.Command, cfg.Args...)
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
	cmd.Stderr = io.Discard
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("mcp %s: starting %s: %w", cfg.Name, cfg.Command, err)
	}

	c := &Client{
		Name:    cfg.Name,
		cmd:     cmd,
		stdin:   stdin,
		pending: map[int64]chan *rpcMsg{},
		closed:  make(chan struct{}),
	}
	go c.readLoop(stdout)
	go func() { _ = cmd.Wait(); c.Close() }()

	initCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	initParams := map[string]any{
		"protocolVersion": protocolVersion,
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "tapioca", "version": "0.1.0"},
	}
	if _, err := c.call(initCtx, "initialize", initParams); err != nil {
		c.Close()
		return nil, fmt.Errorf("mcp %s: initialize: %w", cfg.Name, err)
	}
	if err := c.notify("notifications/initialized", map[string]any{}); err != nil {
		c.Close()
		return nil, fmt.Errorf("mcp %s: %w", cfg.Name, err)
	}

	res, err := c.call(initCtx, "tools/list", map[string]any{})
	if err != nil {
		c.Close()
		return nil, fmt.Errorf("mcp %s: tools/list: %w", cfg.Name, err)
	}
	var list struct {
		Tools []struct {
			Name        string          `json:"name"`
			Description string          `json:"description"`
			InputSchema json.RawMessage `json:"inputSchema"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(res, &list); err != nil {
		c.Close()
		return nil, fmt.Errorf("mcp %s: decoding tools: %w", cfg.Name, err)
	}
	for _, t := range list.Tools {
		c.Tools = append(c.Tools, Tool{Server: cfg.Name, Name: t.Name, Description: t.Description, InputSchema: t.InputSchema})
	}
	return c, nil
}

func (c *Client) readLoop(stdout io.Reader) {
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var msg rpcMsg
		if err := json.Unmarshal(line, &msg); err != nil {
			continue
		}
		switch {
		case msg.Method != "" && msg.ID != nil:
			// Server-initiated request: answer ping, reject the rest.
			if msg.Method == "ping" {
				_ = c.send(rpcMsg{JSONRPC: "2.0", ID: msg.ID, Result: json.RawMessage("{}")})
			} else {
				_ = c.send(rpcMsg{JSONRPC: "2.0", ID: msg.ID, Error: &rpcError{Code: -32601, Message: "method not supported"}})
			}
		case msg.Method != "":
			// Notification; nothing to do.
		default:
			var id int64
			if json.Unmarshal(msg.ID, &id) != nil {
				continue
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
	c.Close()
}

func (c *Client) send(msg rpcMsg) error {
	data, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	_, err = c.stdin.Write(append(data, '\n'))
	return err
}

func (c *Client) notify(method string, params any) error {
	p, err := json.Marshal(params)
	if err != nil {
		return err
	}
	return c.send(rpcMsg{JSONRPC: "2.0", Method: method, Params: p})
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
	if err := c.send(req); err != nil {
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

// Alive reports whether the server process is still running.
func (c *Client) Alive() bool {
	select {
	case <-c.closed:
		return false
	default:
		return true
	}
}

// Close terminates the server process.
func (c *Client) Close() {
	c.closeOnce.Do(func() {
		close(c.closed)
		_ = c.stdin.Close()
		if c.cmd.Process != nil {
			_ = c.cmd.Process.Kill()
		}
	})
}
