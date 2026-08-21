// Package lsp talks to language servers so the agent finds out about the
// mistakes in an edit immediately, instead of when a build eventually runs.
package lsp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"tapioca/internal/config"
	"tapioca/internal/secretenv"
	"tapioca/internal/textenc"
)

// LSP frames messages with headers rather than newlines, so this cannot reuse
// the MCP client.
type rpcMsg struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

// Severity levels as defined by the protocol.
const (
	SeverityError   = 1
	SeverityWarning = 2
	SeverityInfo    = 3
	SeverityHint    = 4
)

// Diagnostic is one problem reported for a file.
type Diagnostic struct {
	Line     int // 1-based, for humans and for the model
	Column   int
	Severity int
	Message  string
	Source   string
}

func (d Diagnostic) String() string {
	label := "error"
	switch d.Severity {
	case SeverityWarning:
		label = "warning"
	case SeverityInfo:
		label = "info"
	case SeverityHint:
		label = "hint"
	}
	src := ""
	if d.Source != "" {
		src = " (" + d.Source + ")"
	}
	return fmt.Sprintf("%d:%d %s: %s%s", d.Line, d.Column, label, d.Message, src)
}

// Client is one running language server.
type Client struct {
	Name string
	Exts []string

	cmd   *exec.Cmd
	stdin io.WriteCloser

	writeMu sync.Mutex

	mu       sync.Mutex
	nextID   int64
	pending  map[int64]chan *rpcMsg
	diags    map[string][]Diagnostic // by URI; the server replaces the whole set
	fresh    map[string]chan struct{}
	opened   map[string]int // URI -> document version
	closed   chan struct{}
	closeOne sync.Once
}

// Start launches a server and completes the handshake against root.
func Start(ctx context.Context, cfg config.LSPServerConfig, root string) (*Client, error) {
	cmd := exec.Command(cfg.Command, cfg.Args...)
	cmd.Dir = root
	cmd.Env = secretenv.Scrubbed()
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
		return nil, fmt.Errorf("lsp %s: starting %s: %w", cfg.Name, cfg.Command, err)
	}

	c := &Client{
		Name: cfg.Name, Exts: cfg.Extensions,
		cmd: cmd, stdin: stdin,
		pending: map[int64]chan *rpcMsg{},
		diags:   map[string][]Diagnostic{},
		fresh:   map[string]chan struct{}{},
		opened:  map[string]int{},
		closed:  make(chan struct{}),
	}
	go func() {
		c.readLoop(stdout)
		c.Close()
	}()

	initCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if _, err := c.call(initCtx, "initialize", map[string]any{
		"processId": nil,
		"rootUri":   pathToURI(root),
		"capabilities": map[string]any{
			"textDocument": map[string]any{
				"publishDiagnostics": map[string]any{},
				"synchronization":    map[string]any{"didSave": true},
			},
		},
		"workspaceFolders": []any{
			map[string]any{"uri": pathToURI(root), "name": filepath.Base(root)},
		},
	}); err != nil {
		c.Close()
		return nil, fmt.Errorf("lsp %s: initialize: %w", cfg.Name, err)
	}
	if err := c.notify("initialized", map[string]any{}); err != nil {
		c.Close()
		return nil, err
	}
	return c, nil
}

// Handles reports whether this server covers a file.
func (c *Client) Handles(path string) bool {
	ext := strings.ToLower(filepath.Ext(path))
	for _, e := range c.Exts {
		if strings.ToLower(e) == ext {
			return true
		}
	}
	return false
}

// Sync tells the server the current contents of a file, opening it the first
// time and reporting a change afterwards.
func (c *Client) Sync(path, text string) error {
	uri := pathToURI(path)
	c.mu.Lock()
	version, already := c.opened[uri]
	version++
	c.opened[uri] = version
	// A fresh channel per sync: waiters must not see diagnostics published
	// for the previous version.
	c.fresh[uri] = make(chan struct{})
	c.mu.Unlock()

	if !already {
		return c.notify("textDocument/didOpen", map[string]any{
			"textDocument": map[string]any{
				"uri": uri, "languageId": languageID(path), "version": version, "text": text,
			},
		})
	}
	return c.notify("textDocument/didChange", map[string]any{
		"textDocument":   map[string]any{"uri": uri, "version": version},
		"contentChanges": []any{map[string]any{"text": text}}, // full sync
	})
}

// Diagnostics waits briefly for the server to report on a file. Servers push
// asynchronously and a cold one may still be loading the workspace, so this
// returns what it has when the wait runs out rather than blocking an edit.
func (c *Client) Diagnostics(path string, wait time.Duration) []Diagnostic {
	uri := pathToURI(path)
	c.mu.Lock()
	ch := c.fresh[uri]
	c.mu.Unlock()

	if ch != nil {
		select {
		case <-ch:
		case <-time.After(wait):
		case <-c.closed:
		}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]Diagnostic(nil), c.diags[uri]...)
}

func (c *Client) readLoop(stdout io.Reader) {
	r := bufio.NewReader(stdout)
	for {
		body, err := readFrame(r)
		if err != nil {
			return
		}
		var msg rpcMsg
		if json.Unmarshal(body, &msg) != nil {
			continue
		}
		switch {
		case msg.Method == "textDocument/publishDiagnostics":
			c.handleDiagnostics(msg.Params)
		case msg.Method != "" && msg.ID != nil:
			// Server-to-client request: decline politely rather than hang it.
			_ = c.write(rpcMsg{JSONRPC: "2.0", ID: msg.ID, Result: json.RawMessage("null")})
		case msg.Method != "":
			// Other notifications (progress, logs) are not needed here.
		default:
			var id int64
			if json.Unmarshal(msg.ID, &id) != nil {
				continue
			}
			c.mu.Lock()
			ch := c.pending[id]
			delete(c.pending, id)
			c.mu.Unlock()
			if ch != nil {
				m := msg
				ch <- &m
			}
		}
	}
}

// clip trims s to n bytes without splitting a rune, and says it did.
func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return textenc.Cut(s, n) + "…"
}

func (c *Client) handleDiagnostics(params json.RawMessage) {
	var p struct {
		URI         string `json:"uri"`
		Diagnostics []struct {
			Range struct {
				Start struct {
					Line      int `json:"line"`
					Character int `json:"character"`
				} `json:"start"`
			} `json:"range"`
			Severity int    `json:"severity"`
			Message  string `json:"message"`
			Source   string `json:"source"`
		} `json:"diagnostics"`
	}
	if json.Unmarshal(params, &p) != nil {
		return
	}
	n := len(p.Diagnostics)
	if n > maxStoredDiags {
		n = maxStoredDiags
	}
	list := make([]Diagnostic, 0, n)
	for _, d := range p.Diagnostics[:n] {
		sev := d.Severity
		if sev == 0 {
			sev = SeverityError // the protocol lets servers omit it
		}
		list = append(list, Diagnostic{
			Line:     d.Range.Start.Line + 1, // LSP counts from zero
			Column:   d.Range.Start.Character + 1,
			Severity: sev,
			Message:  clip(strings.TrimSpace(d.Message), maxDiagBytes),
			Source:   clip(d.Source, 64),
		})
	}
	c.mu.Lock()
	// publishDiagnostics is unsolicited and its URI is the server's to choose,
	// so the map is bounded here; nothing else deletes from it.
	if len(c.diags) > maxTrackedFiles {
		c.diags = map[string][]Diagnostic{}
	}
	c.diags[p.URI] = list
	if ch, ok := c.fresh[p.URI]; ok && ch != nil {
		close(ch)
		c.fresh[p.URI] = nil
	}
	c.mu.Unlock()
}

func (c *Client) write(msg rpcMsg) error {
	msg.JSONRPC = "2.0"
	data, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	_, err = fmt.Fprintf(c.stdin, "Content-Length: %d\r\n\r\n%s", len(data), data)
	return err
}

func (c *Client) notify(method string, params any) error {
	p, err := json.Marshal(params)
	if err != nil {
		return err
	}
	return c.write(rpcMsg{Method: method, Params: p})
}

func (c *Client) call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	p, err := json.Marshal(params)
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	c.nextID++
	id := c.nextID
	ch := make(chan *rpcMsg, 1)
	c.pending[id] = ch
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
	}()

	if err := c.write(rpcMsg{ID: json.RawMessage(strconv.FormatInt(id, 10)), Method: method, Params: p}); err != nil {
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

// Close shuts the server down.
func (c *Client) Close() {
	c.closeOne.Do(func() {
		close(c.closed)
		_ = c.stdin.Close()
		if c.cmd.Process != nil {
			_ = c.cmd.Process.Kill()
		}
	})
}

// A language server is a subprocess that can be wrong or hostile, and both
// look the same from here. Content-Length went straight from Atoi into a
// make(): "Content-Length: 9223372036854775807" panicked in makeslice on a
// goroutine with no recover, which killed Tapioca outright, and a smaller
// absurd value allocated that many bytes for real. Header lines were read with
// no bound either, so one line with no newline in it could be swallowed whole.
const (
	maxFrameBytes  = 32 << 20
	maxHeaderBytes = 8 << 10
	// maxTrackedFiles bounds the diagnostics map, whose keys come from the
	// server. Dropping the lot is fine: diagnostics are re-published per edit.
	maxTrackedFiles = 4096
	// maxDiagBytes clips one diagnostic message. Every other bound here counts
	// things — frames, header bytes, tracked files, and Format's twenty printed
	// problems — and none of them bounds how long one message is. A message is
	// the server's prose about bytes the repository wrote, and servers quote
	// what they are complaining about: a hundred-thousand-character identifier
	// or string literal comes back inside the error about it. Twenty of those
	// are what edit_file appends to its result, which is what goes to the model
	// and what the transcript keeps for the session.
	maxDiagBytes = 2 << 10
	// maxStoredDiags bounds how many problems are kept for one file. Format
	// prints twenty and says how many more there are, so the number above this
	// buys nothing and the set is held per URI, for as many URIs as
	// maxTrackedFiles allows. A file with a thousand problems in it is already
	// a file nobody is reading the count of.
	maxStoredDiags = 1000
)

// readFrame reads one Content-Length framed message.
func readFrame(r *bufio.Reader) ([]byte, error) {
	length := 0
	for {
		line, err := readHeaderLine(r)
		if err != nil {
			return nil, err
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			break // end of headers
		}
		if name, value, ok := strings.Cut(line, ":"); ok &&
			strings.EqualFold(strings.TrimSpace(name), "content-length") {
			length, err = strconv.Atoi(strings.TrimSpace(value))
			if err != nil {
				return nil, fmt.Errorf("bad Content-Length: %w", err)
			}
		}
	}
	if length <= 0 {
		return nil, fmt.Errorf("message with no Content-Length")
	}
	if length > maxFrameBytes {
		return nil, fmt.Errorf("Content-Length %d exceeds the %d byte limit", length, maxFrameBytes)
	}
	body := make([]byte, length)
	if _, err := io.ReadFull(r, body); err != nil {
		return nil, err
	}
	return body, nil
}

// readHeaderLine is ReadString('\n') with a ceiling, so a server that never
// sends one cannot make us buffer without end.
func readHeaderLine(r *bufio.Reader) (string, error) {
	// ReadSlice, not ReadString: ReadString handles a full buffer internally
	// and never returns ErrBufferFull, so the ceiling below was unreachable
	// and a server that never sends a newline buffered without end inside
	// bufio, where nothing here could see it.
	var b strings.Builder
	for {
		chunk, err := r.ReadSlice('\n')
		if b.Len()+len(chunk) > maxHeaderBytes {
			return "", fmt.Errorf("header line exceeds %d bytes", maxHeaderBytes)
		}
		b.Write(chunk)
		if err == nil {
			return b.String(), nil
		}
		if err != bufio.ErrBufferFull {
			return b.String(), err
		}
	}
}

func pathToURI(path string) string {
	abs, err := filepath.Abs(path)
	if err != nil {
		abs = path
	}
	return "file://" + (&url.URL{Path: abs}).EscapedPath()
}

// languageID is a best guess; servers configured for an extension rarely care.
func languageID(path string) string {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".go":
		return "go"
	case ".ts", ".tsx":
		return "typescript"
	case ".js", ".jsx":
		return "javascript"
	case ".py":
		return "python"
	case ".rs":
		return "rust"
	case ".c", ".h":
		return "c"
	case ".cpp", ".cc", ".hpp":
		return "cpp"
	case ".java":
		return "java"
	case ".rb":
		return "ruby"
	case ".zig":
		return "zig"
	default:
		return strings.TrimPrefix(strings.ToLower(filepath.Ext(path)), ".")
	}
}
