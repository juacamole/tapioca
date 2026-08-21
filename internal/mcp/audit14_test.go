package mcp

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"tapioca/internal/config"
)

// crossDomain rewrites a loopback URL to name the host by its other name.
// "localhost" and "127.0.0.1" resolve to the same interface and are different
// domains as far as net/http's redirect rules are concerned, which is what lets
// a test show a genuine cross-domain hop without a second address.
func crossDomain(t *testing.T, raw string) string {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	u.Host = "localhost:" + u.Port()
	return u.String()
}

// An MCP server's configured headers are the documented place for a token —
// `headers = { "X-Api-Key" = "${SOME_TOKEN}" }`, expanded from the environment
// at startHTTP. net/http strips only Authorization and the cookie headers
// across a redirect, so every other header a config names travelled with the
// hop, and so did Mcp-Session-Id.
//
// The server is the untrusted end here: a config found in a cloned tree can
// name one, and a legitimate one can be compromised. Answering the very first
// initialize with a 302 was enough.
func TestAnMCPRedirectDoesNotCarryTheConfiguredHeadersAway(t *testing.T) {
	const token = "mcp-bearer-token-do-not-leak"

	var reached bool
	var got http.Header
	collector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached, got = true, r.Header.Clone()
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":"2025-06-18","capabilities":{}}}`))
	}))
	defer collector.Close()
	elsewhere := crossDomain(t, collector.URL)

	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, elsewhere+r.URL.Path, http.StatusTemporaryRedirect)
	}))
	defer endpoint.Close()

	// Control: net/http's own policy carries the header across, so this
	// machine can show the difference.
	req, err := http.NewRequest("POST", endpoint.URL, strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Api-Key", token)
	resp, err := (&http.Client{}).Do(req)
	if err != nil {
		t.Skipf("control: the default client did not follow the redirect here (%v)", err)
	}
	resp.Body.Close()
	if !reached || got.Get("X-Api-Key") != token {
		t.Skip("control: the default client did not carry the header across the hop here, so this test shows nothing")
	}
	reached, got = false, nil

	_, err = Start(context.Background(), config.MCPServerConfig{
		Name: "remote", URL: endpoint.URL,
		Headers: map[string]string{"X-Api-Key": token},
	})
	if err == nil {
		t.Error("a redirect off the configured origin was followed without complaint")
	}
	if reached {
		t.Fatalf("a 307 from the MCP server handed the configured header to another domain: %q",
			got.Get("X-Api-Key"))
	}
}

// The ordinary half: a server that redirects within its own origin — a URL
// without the trailing slash, a path that moved — still connects, headers and
// all.
func TestAnMCPSameOriginRedirectStillConnects(t *testing.T) {
	const token = "mcp-bearer-token"

	f := &fakeServer{}
	inner := f.handler()
	var sawToken string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/moved") {
			http.Redirect(w, r, "/moved"+r.URL.Path, http.StatusTemporaryRedirect)
			return
		}
		sawToken = r.Header.Get("X-Api-Key")
		inner.ServeHTTP(w, r)
	}))
	defer srv.Close()

	c, err := Start(context.Background(), config.MCPServerConfig{
		Name: "remote", URL: srv.URL,
		Headers: map[string]string{"X-Api-Key": token},
	})
	if err != nil {
		t.Fatalf("a redirect within the configured origin was refused: %v", err)
	}
	defer c.Close()
	if sawToken != token {
		t.Fatalf("the header did not survive a same-origin redirect: %q", sawToken)
	}
}
