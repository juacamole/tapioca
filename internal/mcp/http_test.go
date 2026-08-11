package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"tapioca/internal/config"
)

// fakeServer is a minimal streamable-HTTP MCP server. sse switches the reply
// format; both are legal and clients must handle either.
type fakeServer struct {
	sse bool

	mu      sync.Mutex
	auth    []string // Authorization header seen per request
	session []string // Mcp-Session-Id seen per request
	deleted bool
}

func (f *fakeServer) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.auth = append(f.auth, r.Header.Get("Authorization"))
		f.session = append(f.session, r.Header.Get("Mcp-Session-Id"))
		if r.Method == http.MethodDelete {
			f.deleted = true
			f.mu.Unlock()
			w.WriteHeader(http.StatusOK)
			return
		}
		f.mu.Unlock()

		var req rpcMsg
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if req.ID == nil { // notification
			w.WriteHeader(http.StatusAccepted)
			return
		}

		var result string
		switch req.Method {
		case "initialize":
			w.Header().Set("Mcp-Session-Id", "sess-42")
			result = `{"protocolVersion":"2024-11-05","capabilities":{}}`
		case "tools/list":
			result = `{"tools":[{"name":"echo","description":"echoes","inputSchema":{"type":"object"}}]}`
		case "tools/call":
			result = `{"content":[{"type":"text","text":"pong"}],"isError":false}`
		default:
			result = `{}`
		}
		body := fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"result":%s}`, req.ID, result)
		if f.sse {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			fmt.Fprintf(w, ": keep-alive\n\nevent: message\ndata: %s\n\n", body)
			if fl, ok := w.(http.Flusher); ok {
				fl.Flush()
			}
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, body)
	})
}

func TestHTTPTransport(t *testing.T) {
	for _, sse := range []bool{false, true} {
		name := "json"
		if sse {
			name = "sse"
		}
		t.Run(name, func(t *testing.T) {
			fake := &fakeServer{sse: sse}
			srv := httptest.NewServer(fake.handler())
			defer srv.Close()

			t.Setenv("TEST_MCP_TOKEN", "s3cret")
			c, err := Start(context.Background(), config.MCPServerConfig{
				Name:    "remote",
				URL:     srv.URL,
				Headers: map[string]string{"Authorization": "Bearer ${TEST_MCP_TOKEN}"},
			})
			if err != nil {
				t.Fatalf("connecting: %v", err)
			}
			defer c.Close()

			if len(c.Tools()) != 1 || c.Tools()[0].Name != "echo" {
				t.Fatalf("tools not listed: %+v", c.Tools())
			}
			out, isErr, err := c.CallTool(context.Background(), "echo", json.RawMessage(`{"x":1}`))
			if err != nil || isErr || out != "pong" {
				t.Fatalf("CallTool = %q, %v, %v", out, isErr, err)
			}

			fake.mu.Lock()
			auth, sessions := fake.auth, fake.session
			fake.mu.Unlock()
			for i, a := range auth {
				if a != "Bearer s3cret" {
					t.Fatalf("request %d sent Authorization %q; ${VAR} not expanded", i, a)
				}
			}
			// The id issued at initialize must be echoed on later requests.
			if len(sessions) < 3 || sessions[0] != "" || sessions[len(sessions)-1] != "sess-42" {
				t.Fatalf("session id not carried: %#v", sessions)
			}
		})
	}
}

func TestHTTPErrorsSurface(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope, bad token", http.StatusUnauthorized)
	}))
	defer srv.Close()

	_, err := Start(context.Background(), config.MCPServerConfig{Name: "remote", URL: srv.URL})
	if err == nil {
		t.Fatal("a 401 should fail the connection")
	}
	if !strings.Contains(err.Error(), "401") || !strings.Contains(err.Error(), "bad token") {
		t.Fatalf("error hides the cause: %v", err)
	}
}

func TestHTTPCloseEndsSession(t *testing.T) {
	fake := &fakeServer{}
	srv := httptest.NewServer(fake.handler())
	defer srv.Close()

	c, err := Start(context.Background(), config.MCPServerConfig{Name: "remote", URL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	c.Close()
	if c.Alive() {
		t.Error("client still reports alive after Close")
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if !fake.deleted {
		t.Error("Close did not release the server-side session")
	}
}
