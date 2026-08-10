// Package acp serves Tapioca as an Agent Client Protocol agent, so editors
// (Zed and friends) can drive it over stdio instead of the TUI.
package acp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"sync"
)

// protocolVersion is the ACP revision this implementation speaks.
const protocolVersion = 1

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

// conn is a bidirectional JSON-RPC 2.0 connection over a pipe. Both sides send
// requests, so replies have to be routed back to whoever is waiting.
type conn struct {
	w  *bufio.Writer
	mu sync.Mutex

	pendingMu sync.Mutex
	pending   map[int64]chan *rpcMsg
	nextID    int64
}

func newConn(w io.Writer) *conn {
	return &conn{w: bufio.NewWriter(w), pending: map[int64]chan *rpcMsg{}}
}

func (c *conn) write(msg rpcMsg) error {
	msg.JSONRPC = "2.0"
	data, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, err := c.w.Write(append(data, '\n')); err != nil {
		return err
	}
	return c.w.Flush()
}

func (c *conn) notify(method string, params any) error {
	p, err := json.Marshal(params)
	if err != nil {
		return err
	}
	return c.write(rpcMsg{Method: method, Params: p})
}

// call sends a request to the client and waits for its reply.
func (c *conn) call(ctx context.Context, method string, params any) (json.RawMessage, error) {
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

	if err := c.write(rpcMsg{ID: json.RawMessage(strconv.FormatInt(id, 10)), Method: method, Params: p}); err != nil {
		return nil, err
	}
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case resp := <-ch:
		if resp.Error != nil {
			return nil, fmt.Errorf("%s (code %d)", resp.Error.Message, resp.Error.Code)
		}
		return resp.Result, nil
	}
}

// routeResponse hands a reply to the caller waiting on it.
func (c *conn) routeResponse(msg *rpcMsg) {
	var id int64
	if json.Unmarshal(msg.ID, &id) != nil {
		return
	}
	c.pendingMu.Lock()
	ch := c.pending[id]
	delete(c.pending, id)
	c.pendingMu.Unlock()
	if ch != nil {
		ch <- msg
	}
}

func (c *conn) respond(id json.RawMessage, result any) {
	r, err := json.Marshal(result)
	if err != nil {
		c.fail(id, -32603, err.Error())
		return
	}
	_ = c.write(rpcMsg{ID: id, Result: r})
}

func (c *conn) fail(id json.RawMessage, code int, message string) {
	_ = c.write(rpcMsg{ID: id, Error: &rpcError{Code: code, Message: message}})
}
