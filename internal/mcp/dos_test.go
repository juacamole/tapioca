package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"tapioca/internal/config"
)

// A server that answers every POST with a request of its own. Replying inline
// re-entered Send from inside Send and recursed until the stack blew.
type replyToReplyServer struct{ hits chan struct{} }

func (s *replyToReplyServer) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req rpcMsg
		if json.NewDecoder(r.Body).Decode(&req) != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		select {
		case s.hits <- struct{}{}:
		default:
		}
		w.Header().Set("Content-Type", "application/json")
		if req.Method == "initialize" {
			fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"result":{"protocolVersion":%q,"capabilities":{}}}`,
				req.ID, protocolVersion)
			return
		}
		if req.Method == "tools/list" {
			fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"result":{"tools":[]}}`, req.ID)
			return
		}
		// Everything else, including our replies, gets a request back.
		fmt.Fprint(w, `{"jsonrpc":"2.0","id":9999,"method":"ping"}`)
	})
}

func TestReplyToReplyDoesNotRecurse(t *testing.T) {
	s := &replyToReplyServer{hits: make(chan struct{}, 1)}
	srv := httptest.NewServer(s.handler())
	defer srv.Close()
	c, err := Start(context.Background(), config.MCPServerConfig{Name: "hostile", URL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	before := runtime.NumGoroutine()
	done := make(chan struct{})
	go func() {
		defer close(done)
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_, _, _ = c.CallTool(ctx, "anything", nil)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("CallTool never returned; the reply loop is wedged")
	}
	// Replies are fired and forgotten, so a few goroutines may still be in
	// flight — what must not happen is thousands.
	time.Sleep(200 * time.Millisecond)
	if grew := runtime.NumGoroutine() - before; grew > 100 {
		t.Fatalf("goroutines grew by %d answering one hostile server", grew)
	}
}

// A server that floods list_changed spawned one goroutine and one pending
// request per notification.
func TestListChangedFloodIsCoalesced(t *testing.T) {
	var listed int32
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
			mu.Lock()
			listed++
			mu.Unlock()
			time.Sleep(20 * time.Millisecond) // a real list is not instant
			result = `{"tools":[{"name":"t","description":"d","inputSchema":{}}]}`
		default:
			result = `{}`
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"result":%s}`, req.ID, result)
	}))
	defer srv.Close()

	c, err := Start(context.Background(), config.MCPServerConfig{Name: "flood", URL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	before := runtime.NumGoroutine()
	for i := 0; i < 5000; i++ {
		c.handle([]byte(`{"jsonrpc":"2.0","method":"notifications/tools/list_changed"}`))
	}
	if grew := runtime.NumGoroutine() - before; grew > 50 {
		t.Fatalf("5000 notifications spawned %d goroutines", grew)
	}
	// The refresh still has to happen, and a change arriving mid-refresh must
	// not be dropped: one in flight plus one queued.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		n := listed
		mu.Unlock()
		if n >= 2 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("the flood was coalesced away entirely; no refresh happened")
}

// data: lines with no blank line between them accumulated without limit.
func TestSSEFloodIsBounded(t *testing.T) {
	stop := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req rpcMsg
		if json.NewDecoder(r.Body).Decode(&req) != nil {
			return
		}
		// The handshake is answered normally; only the tool call floods, so
		// the test measures the flood and not a failed connection.
		switch req.Method {
		case "initialize":
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"result":{"protocolVersion":%q,"capabilities":{}}}`,
				req.ID, protocolVersion)
			return
		case "tools/list":
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"result":{"tools":[]}}`, req.ID)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		chunk := "data: " + strings.Repeat("A", 64<<10) + "\n"
		for {
			select {
			case <-stop:
				return
			default:
			}
			if _, err := fmt.Fprint(w, chunk); err != nil {
				return
			}
			if fl, ok := w.(http.Flusher); ok {
				fl.Flush()
			}
		}
	}))
	defer srv.Close()
	defer close(stop)

	c, err := Start(context.Background(), config.MCPServerConfig{Name: "flood", URL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	var m0, m1 runtime.MemStats
	runtime.ReadMemStats(&m0)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, _, _ = c.CallTool(ctx, "anything", nil)
	time.Sleep(500 * time.Millisecond)
	runtime.ReadMemStats(&m1)
	if grew := int64(m1.HeapInuse) - int64(m0.HeapInuse); grew > 256<<20 {
		t.Fatalf("heap grew %d MB buffering one event", grew>>20)
	}
}

// Tens of thousands of small content parts made the join cost more than the
// call, and nothing downstream bounded the result.
func TestToolResultJoinIsBoundedAndLinear(t *testing.T) {
	parts := make([]string, 60_000)
	for i := range parts {
		parts[i] = `{"type":"text","text":"xxxxxxxxxxxxxxxxxxxx"}`
	}
	body := `{"content":[` + strings.Join(parts, ",") + `]}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req rpcMsg
		if json.NewDecoder(r.Body).Decode(&req) != nil {
			return
		}
		var result string
		switch req.Method {
		case "initialize":
			result = fmt.Sprintf(`{"protocolVersion":%q,"capabilities":{}}`, protocolVersion)
		case "tools/list":
			result = `{"tools":[]}`
		default:
			result = body
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"result":%s}`, req.ID, result)
	}))
	defer srv.Close()

	c, err := Start(context.Background(), config.MCPServerConfig{Name: "big", URL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	start := time.Now()
	out, _, err := c.CallTool(context.Background(), "anything", nil)
	if err != nil {
		t.Fatal(err)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("joining 60k parts took %v", d)
	}
	if len(out) > maxToolOutput+64 {
		t.Fatalf("result was %d bytes, over the %d cap", len(out), maxToolOutput)
	}
}
