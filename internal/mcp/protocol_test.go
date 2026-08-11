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
	"time"

	"tapioca/internal/config"
)

// versionServer answers initialize with whatever revision it is told to, and
// records the protocol header of every request.
type versionServer struct {
	answer string

	mu       sync.Mutex
	versions []string
	offered  string
}

func (v *versionServer) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		v.mu.Lock()
		v.versions = append(v.versions, r.Header.Get("MCP-Protocol-Version"))
		v.mu.Unlock()

		var req rpcMsg
		if json.NewDecoder(r.Body).Decode(&req) != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if req.ID == nil {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		var result string
		switch req.Method {
		case "initialize":
			var p struct {
				ProtocolVersion string `json:"protocolVersion"`
			}
			json.Unmarshal(req.Params, &p)
			v.mu.Lock()
			v.offered = p.ProtocolVersion
			v.mu.Unlock()
			result = fmt.Sprintf(`{"protocolVersion":%q,"capabilities":{}}`, v.answer)
		case "tools/list":
			result = `{"tools":[{"name":"echo","description":"d","inputSchema":{"type":"object"}}]}`
		default:
			result = `{}`
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"result":%s}`, req.ID, result)
	})
}

// The client offers the newest revision it knows.
func TestHandshakeOffersTheNewestRevision(t *testing.T) {
	v := &versionServer{answer: protocolVersion}
	srv := httptest.NewServer(v.handler())
	defer srv.Close()
	c, err := Start(context.Background(), config.MCPServerConfig{Name: "s", URL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	v.mu.Lock()
	offered := v.offered
	v.mu.Unlock()
	if offered != protocolVersions[0] {
		t.Fatalf("offered %q, want the newest known revision %q", offered, protocolVersions[0])
	}
	if c.Version() != protocolVersion {
		t.Fatalf("settled on %q", c.Version())
	}
}

// Most servers in the wild still answer with the oldest revision, and dropping
// them would be a regression rather than an upgrade.
func TestHandshakeAcceptsAnOlderRevision(t *testing.T) {
	v := &versionServer{answer: "2024-11-05"}
	srv := httptest.NewServer(v.handler())
	defer srv.Close()
	c, err := Start(context.Background(), config.MCPServerConfig{Name: "s", URL: srv.URL})
	if err != nil {
		t.Fatalf("refused a server on an older revision: %v", err)
	}
	defer c.Close()
	if c.Version() != "2024-11-05" {
		t.Fatalf("settled on %q, want the revision the server named", c.Version())
	}
	if len(c.Tools()) != 1 {
		t.Fatalf("tools not listed: %+v", c.Tools())
	}
}

// Guessing at a revision nobody has implemented yet is worse than saying so.
func TestHandshakeRejectsAnUnknownRevision(t *testing.T) {
	v := &versionServer{answer: "2030-01-01"}
	srv := httptest.NewServer(v.handler())
	defer srv.Close()
	_, err := Start(context.Background(), config.MCPServerConfig{Name: "s", URL: srv.URL})
	if err == nil {
		t.Fatal("connected to a server speaking an unknown revision")
	}
	if !strings.Contains(err.Error(), "2030-01-01") {
		t.Fatalf("error does not name the revision: %v", err)
	}
}

// Required from 2025-06-18 on every request after initialize.
func TestProtocolVersionHeaderSentAfterInitialize(t *testing.T) {
	v := &versionServer{answer: protocolVersion}
	srv := httptest.NewServer(v.handler())
	defer srv.Close()
	c, err := Start(context.Background(), config.MCPServerConfig{Name: "s", URL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	v.mu.Lock()
	seen := append([]string(nil), v.versions...)
	v.mu.Unlock()
	if len(seen) < 2 {
		t.Fatalf("expected more than the initialize request: %v", seen)
	}
	if seen[0] != "" {
		t.Errorf("initialize carried a version header before one was agreed: %q", seen[0])
	}
	for i, got := range seen[1:] {
		if got != protocolVersion {
			t.Errorf("request %d carried MCP-Protocol-Version %q, want %q", i+1, got, protocolVersion)
		}
	}
}

// changingServer swaps its tool list after the first tools/list and announces
// it, the way a gateway fronting another backend does.
type changingServer struct {
	mu     sync.Mutex
	listed int
}

func (s *changingServer) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req rpcMsg
		if json.NewDecoder(r.Body).Decode(&req) != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if req.ID == nil {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		var result string
		switch req.Method {
		case "initialize":
			result = fmt.Sprintf(`{"protocolVersion":%q,"capabilities":{"tools":{"listChanged":true}}}`, protocolVersion)
		case "tools/list":
			s.mu.Lock()
			s.listed++
			n := s.listed
			s.mu.Unlock()
			if n == 1 {
				result = `{"tools":[{"name":"before","description":"d","inputSchema":{}}]}`
			} else {
				result = `{"tools":[{"name":"after","description":"d","inputSchema":{}},` +
					`{"name":"extra","description":"d","inputSchema":{}}]}`
			}
		default:
			result = `{}`
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"result":%s}`, req.ID, result)
	})
}

// Without this the server keeps being offered its old tools until restart.
func TestToolsListChangedRefreshesTheList(t *testing.T) {
	s := &changingServer{}
	srv := httptest.NewServer(s.handler())
	defer srv.Close()
	c, err := Start(context.Background(), config.MCPServerConfig{Name: "s", URL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if len(c.Tools()) != 1 || c.Tools()[0].Name != "before" {
		t.Fatalf("unexpected initial tools: %+v", c.Tools())
	}

	changed := make(chan struct{}, 1)
	c.OnToolsChanged(func() { changed <- struct{}{} })
	c.handle([]byte(`{"jsonrpc":"2.0","method":"notifications/tools/list_changed"}`))

	select {
	case <-changed:
	case <-time.After(5 * time.Second):
		t.Fatal("no refresh after tools/list_changed")
	}
	tools := c.Tools()
	if len(tools) != 2 || tools[0].Name != "after" {
		t.Fatalf("tools not replaced: %+v", tools)
	}
}
