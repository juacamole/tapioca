package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"tapioca/internal/config"
)

// httpTransport speaks the streamable HTTP transport: every message is POSTed
// to one endpoint, and the reply is either a JSON body or an SSE stream.
type httpTransport struct {
	url     string
	headers map[string]string
	client  *http.Client
	c       *Client

	mu      sync.Mutex
	session string // Mcp-Session-Id, issued at initialize
	version string // negotiated revision, sent from the next request on
}

func (h *httpTransport) setVersion(v string) {
	h.mu.Lock()
	h.version = v
	h.mu.Unlock()
}

func (c *Client) startHTTP(cfg config.MCPServerConfig) error {
	headers := make(map[string]string, len(cfg.Headers))
	for k, v := range cfg.Headers {
		headers[k] = expandEnv(v)
	}
	c.tr = &httpTransport{
		url:     strings.TrimSpace(cfg.URL),
		headers: headers,
		// No overall timeout: SSE responses stay open. Per-request contexts
		// bound each call instead.
		client: &http.Client{Timeout: 0},
		c:      c,
	}
	return nil
}

// expandEnv resolves ${VAR} so tokens can live in the environment instead of
// the config file.
func expandEnv(v string) string {
	return os.Expand(v, func(name string) string { return os.Getenv(name) })
}

func (h *httpTransport) Send(ctx context.Context, data []byte) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, h.url, bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	for k, v := range h.headers {
		req.Header.Set(k, v)
	}
	h.mu.Lock()
	session, version := h.session, h.version
	h.mu.Unlock()
	if session != "" {
		req.Header.Set("Mcp-Session-Id", session)
	}
	// Required from revision 2025-06-18 on every request after initialize;
	// harmless to a server on an older revision.
	if version != "" {
		req.Header.Set("MCP-Protocol-Version", version)
	}

	resp, err := h.client.Do(req)
	if err != nil {
		return err
	}
	if sid := resp.Header.Get("Mcp-Session-Id"); sid != "" {
		h.mu.Lock()
		h.session = sid
		h.mu.Unlock()
	}
	if resp.StatusCode == http.StatusAccepted || resp.StatusCode == http.StatusNoContent {
		resp.Body.Close() // notification, nothing comes back
		return nil
	}
	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		resp.Body.Close()
		return fmt.Errorf("%s: %s", resp.Status, strings.TrimSpace(string(body)))
	}
	if strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		// The reply arrives on a stream that may stay open; the caller is
		// already waiting on its pending channel, so read it in the background.
		go h.readSSE(resp.Body)
		return nil
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 16*1024*1024))
	if err != nil {
		return err
	}
	h.dispatch(body)
	return nil
}

// dispatch feeds a JSON body to the client; servers may batch into an array.
func (h *httpTransport) dispatch(body []byte) {
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 {
		return
	}
	if trimmed[0] == '[' {
		var batch []json.RawMessage
		if json.Unmarshal(trimmed, &batch) == nil {
			for _, m := range batch {
				h.c.handle(m)
			}
			return
		}
	}
	h.c.handle(trimmed)
}

// readSSE parses text/event-stream and dispatches every data payload. Only the
// data field matters here: ids and event names carry no JSON-RPC meaning.
func (h *httpTransport) readSSE(body io.ReadCloser) {
	defer body.Close()
	sc := bufio.NewScanner(body)
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	var data []string
	flush := func() {
		if len(data) == 0 {
			return
		}
		h.dispatch([]byte(strings.Join(data, "\n")))
		data = data[:0]
	}
	for sc.Scan() {
		select {
		case <-h.c.closed:
			return
		default:
		}
		line := strings.TrimSuffix(sc.Text(), "\r")
		switch {
		case line == "":
			flush()
		case strings.HasPrefix(line, ":"):
			// comment / keep-alive
		case strings.HasPrefix(line, "data:"):
			data = append(data, strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
		}
	}
	flush()
}

func (h *httpTransport) Close() {
	h.mu.Lock()
	session, url := h.session, h.url
	h.session = ""
	h.mu.Unlock()
	if session == "" {
		return
	}
	// Best effort: tell the server the session is over so it can free it.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, url, nil)
	if err != nil {
		return
	}
	for k, v := range h.headers {
		req.Header.Set(k, v)
	}
	req.Header.Set("Mcp-Session-Id", session)
	if resp, err := h.client.Do(req); err == nil {
		resp.Body.Close()
	}
}
