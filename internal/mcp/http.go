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
	"tapioca/internal/httpsafe"
)

// maxBodyBytes caps one non-streaming response body, matching the stdio
// scanner's line cap so the same message is oversized on either transport.
const maxBodyBytes = 16 << 20

// httpTransport speaks the streamable HTTP transport: every message is POSTed
// to one endpoint, and the reply is either a JSON body or an SSE stream.
type httpTransport struct {
	url     string
	headers map[string]string
	auth    *oauthSource // nil unless the entry is auth = "oauth"
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
	h := &httpTransport{
		url:     strings.TrimSpace(cfg.URL),
		headers: headers,
		// No overall timeout: SSE responses stay open. Per-request contexts
		// bound each call instead. CheckRedirect is what keeps the configured
		// headers — the documented place for an ${API_TOKEN} — and the
		// Mcp-Session-Id from being carried to whatever host the server's 302
		// names; net/http strips only Authorization and the cookie headers.
		client: &http.Client{Timeout: 0, CheckRedirect: httpsafe.SameOrigin},
		c:      c,
	}
	if UsesOAuth(cfg) {
		src, err := newOAuthSource(cfg)
		if err != nil {
			return err
		}
		h.auth = src
	}
	c.tr = h
	return nil
}

// expandEnv resolves ${VAR} so tokens can live in the environment instead of
// the config file.
func expandEnv(v string) string {
	return os.Expand(v, func(name string) string { return os.Getenv(name) })
}

func (h *httpTransport) Send(ctx context.Context, data []byte) error {
	resp, tok, err := h.post(ctx, data)
	if err != nil {
		return err
	}
	// A 401 on a connection that carried a token means the token was retired
	// early: a grant revoked mid-session, or a server expiring them sooner than
	// it said. One retry with a fresh one covers that. A second would be a loop,
	// since nothing between two attempts can change a refusal that repeats.
	if h.auth != nil && resp.StatusCode == http.StatusUnauthorized {
		resp.Body.Close()
		h.auth.invalidate(tok)
		if resp, _, err = h.post(ctx, data); err != nil {
			return err
		}
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
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes+1))
	if err != nil {
		return err
	}
	// One byte over the cap is reported, not truncated. A truncated body is
	// never valid JSON, so dispatch dropped it without a word and the caller
	// sat on its pending channel until its own deadline — thirty seconds of a
	// wedged turn for a reply the transport had already decided to throw away.
	// stdio answers the same question by tearing the connection down the moment
	// a line exceeds its scanner, and the caller gets "server exited" at once;
	// the two transports must not disagree about what an oversized message is.
	if len(body) > maxBodyBytes {
		return fmt.Errorf("response body exceeds %d bytes", maxBodyBytes)
	}
	h.dispatch(body)
	return nil
}

// post sends one message and reports the token it carried, so a 401 retires
// exactly that token rather than one another goroutine has since obtained.
func (h *httpTransport) post(ctx context.Context, data []byte) (*http.Response, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, h.url, bytes.NewReader(data))
	if err != nil {
		return nil, "", err
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
	tok, err := h.authorize(ctx, req)
	if err != nil {
		return nil, "", err
	}

	resp, err := h.client.Do(req)
	if err != nil {
		return nil, "", err
	}
	if sid := resp.Header.Get("Mcp-Session-Id"); sid != "" {
		h.mu.Lock()
		h.session = sid
		h.mu.Unlock()
	}
	return resp, tok, nil
}

// authorize puts the OAuth token on the request, after the configured headers
// so an Authorization left in the config cannot shadow it — a stale header
// there would otherwise send the request as somebody else, or as nobody.
func (h *httpTransport) authorize(ctx context.Context, req *http.Request) (string, error) {
	if h.auth == nil {
		return "", nil
	}
	tok, err := h.auth.token(ctx)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	return tok, nil
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
	// The scanner's cap bounds one line. An event is every line until a blank
	// one, so a server that sends data: forever and never a blank line
	// accumulated without limit — 5GB in five seconds.
	const maxEventBytes = 16 << 20
	var data []string
	size := 0
	flush := func() {
		if len(data) == 0 {
			return
		}
		h.dispatch([]byte(strings.Join(data, "\n")))
		data, size = data[:0], 0
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
			payload := strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " ")
			if size+len(payload) > maxEventBytes {
				return // no event this large is real; the connection is done
			}
			data = append(data, payload)
			size += len(payload)
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
	// An authorized server will not drop a session for an unauthenticated
	// caller, but a token that cannot be had is no reason to skip the attempt:
	// the request is best effort either way.
	_, _ = h.authorize(ctx, req)
	if resp, err := h.client.Do(req); err == nil {
		resp.Body.Close()
	}
}
