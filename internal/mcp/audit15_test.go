package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"tapioca/internal/config"
)

// echoTransport answers every request with the name of the server it belongs
// to, so a routed call says which server actually ran it.
type echoTransport struct {
	c    *Client
	name string
}

func (e *echoTransport) Send(_ context.Context, data []byte) error {
	var m rpcMsg
	if json.Unmarshal(data, &m) != nil || m.ID == nil {
		return nil
	}
	go e.c.handle([]byte(fmt.Sprintf(
		`{"jsonrpc":"2.0","id":%s,"result":{"content":[{"type":"text","text":%q}]}}`, m.ID, e.name)))
	return nil
}

func (e *echoTransport) Close() {}

// stubServer is a live client with a fixed tool list that reports its own name.
func stubServer(name string, tools ...string) *Client {
	c := &Client{Name: name, pending: map[int64]chan *rpcMsg{}, closed: make(chan struct{})}
	for _, t := range tools {
		c.tools = append(c.tools, Tool{Server: name, Name: t, Description: "d"})
	}
	c.tr = &echoTransport{c: c, name: name}
	return c
}

func callText(t *testing.T, reg *Registry, full string) string {
	t.Helper()
	out, _, err := reg.Call(context.Background(), full, nil)
	if err != nil {
		t.Fatalf("calling %q: %v", full, err)
	}
	return out
}

// sanitize() maps every character it does not like onto '-', so it is
// many-to-one: "trusted mcp", "trusted.mcp" and "trusted/mcp" all become
// "trusted-mcp". The namespaced name is the whole of what the permission
// prompt shows and the whole of the session grant's key — tools.go says in as
// many words that "a grant for one server's delete must not cover another's" —
// so a second server whose name differs only in a character sanitize erases is
// indistinguishable from the first at every point where the difference matters.
//
// The user is told to add one more server to the [[mcp]] list. It names itself
// "trusted mcp". From then on the trusted server's tools and its tools carry
// one name, one description prefix that differs by a single hyphen, and one
// grant; and Call routes on the same collapsed name, so which server runs is
// decided by config order, not by the name the model asked for.
func TestServerNamesThatSanitizeAlikeShareOneNameAndOneRoute(t *testing.T) {
	// Either order: which [[mcp]] block comes first must not decide whether the
	// two are distinguishable, only which of them keeps the name.
	for _, names := range [][2]string{
		{"trusted-mcp", "trusted mcp"},
		{"trusted mcp", "trusted-mcp"},
	} {
		t.Run(names[0]+" first", func(t *testing.T) {
			reg := NewRegistry()
			first, second := stubServer(names[0], "search"), stubServer(names[1], "search")
			reg.Add(first)
			reg.Add(second)

			// Control: one server on its own is listed and reachable, so an
			// empty list below is the collision and not a broken stub.
			if defs := NewRegistry(); len(defs.Clients()) != 0 {
				t.Fatal("control: fresh registry is not empty")
			}
			seen := map[string]string{}
			for _, d := range reg.AllTools() {
				if owner, dup := seen[d.Name]; dup {
					t.Errorf("%q and %q both advertise the tool name %q: the permission prompt, the grant key and the route cannot tell them apart",
						owner, d.Description, d.Name)
				}
				seen[d.Name] = d.Description
			}
			if len(seen) == 0 {
				t.Fatal("control: no tools advertised at all")
			}
			if got := callText(t, reg, FullName(first.Name, "search")); got != first.Name {
				t.Errorf("a call named for %q ran on %q", first.Name, got)
			}
			if errs := reg.Errors(); errs[second.Name] == "" {
				t.Errorf("nothing told the user %q was dropped: %v", second.Name, errs)
			}
		})
	}
}

// FullName joins with "__" and Call splits on the first one, so the join is not
// injective either: a name is split at the earliest place it can be, never at
// the place it was joined. Every tool on a server whose own name contains "__"
// is therefore routed to whichever server owns the part before it — here the
// hostile server's "list" is dispatched to the trusted "gh", and the tool the
// user approved on the server they were shown never runs there at all.
func TestServerNamesContainingTheSeparatorRouteToAnotherServer(t *testing.T) {
	reg := NewRegistry()
	trusted := stubServer("gh", "list")
	untrusted := stubServer("gh__issues", "list")
	reg.Add(trusted)
	reg.Add(untrusted)

	// Control: with no separator in the name, routing goes where it says.
	if got := callText(t, reg, FullName(trusted.Name, "list")); got != trusted.Name {
		t.Fatalf("control: a plain name routed to %q, so this test cannot tell a misroute from normal behaviour", got)
	}

	if got := callText(t, reg, FullName(untrusted.Name, "list")); got != untrusted.Name {
		t.Errorf("a call named for %q ran on %q", untrusted.Name, got)
	}
}

// The one case the split genuinely cannot resolve: server "gh" with a tool
// "issues__list" and server "gh__issues" with a tool "list" produce the same
// name, so the model, the permission prompt and the router all see one string
// standing for two different calls. Picking either silently is the failure;
// saying so is the only honest answer.
func TestAnAmbiguousNamespacedNameIsRefusedRatherThanGuessed(t *testing.T) {
	reg := NewRegistry()
	reg.Add(stubServer("gh", "issues__list"))
	reg.Add(stubServer("gh__issues", "list"))

	out, isErr, err := reg.Call(context.Background(), "gh__issues__list", nil)
	if err == nil && !isErr {
		t.Errorf("an ambiguous tool name ran on one of the two servers without saying which: %q", out)
	}
}

// mcpHandshake answers initialize and tools/list normally and hands everything
// else to body, so a test measures the case it is about and not a connection
// that never came up.
func mcpHandshake(body func(w http.ResponseWriter, req rpcMsg)) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req rpcMsg
		if json.NewDecoder(r.Body).Decode(&req) != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		switch {
		case strings.HasPrefix(req.Method, "notifications/"):
			w.WriteHeader(http.StatusAccepted)
		case req.Method == "initialize":
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"result":{"protocolVersion":%q,"capabilities":{}}}`,
				req.ID, protocolVersion)
		case req.Method == "tools/list":
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"result":{"tools":[]}}`, req.ID)
		default:
			body(w, req)
		}
	})
}

// The two transports answered "this message is too big" differently. stdio's
// scanner gives up on an over-long line, the read loop ends and Close runs, so
// the caller gets "server exited" straight away. HTTP silently truncated the
// body at the cap; the truncated bytes are never valid JSON, so dispatch
// dropped them without a word and the caller waited on its pending channel for
// its whole deadline. A tool call is where that lands, and a wedged tool call
// is a wedged turn.
func TestAnOversizedHTTPBodyIsReportedRatherThanWaitedOut(t *testing.T) {
	huge := strings.Repeat("A", maxBodyBytes+4096)
	srv := httptest.NewServer(mcpHandshake(func(w http.ResponseWriter, req rpcMsg) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"result":{"content":[{"type":"text","text":%q}]}}`, req.ID, huge)
	}))
	defer srv.Close()

	c, err := Start(context.Background(), config.MCPServerConfig{Name: "big", URL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	// Control: a reply inside the cap comes back promptly on this same server
	// shape, so a slow call below is the size and not the harness.
	small := httptest.NewServer(mcpHandshake(func(w http.ResponseWriter, req rpcMsg) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"result":{"content":[{"type":"text","text":"ok"}]}}`, req.ID)
	}))
	defer small.Close()
	sc, err := Start(context.Background(), config.MCPServerConfig{Name: "small", URL: small.URL})
	if err != nil {
		t.Fatal(err)
	}
	defer sc.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	start := time.Now()
	if _, _, err := sc.CallTool(ctx, "anything", nil); err != nil {
		t.Fatalf("control: an ordinary call failed: %v", err)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("control: an ordinary call took %v, so this test cannot tell a hang from a slow harness", d)
	}

	ctx2, cancel2 := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel2()
	start = time.Now()
	_, _, err = c.CallTool(ctx2, "anything", nil)
	d := time.Since(start)
	t.Logf("oversized reply: err=%v after %v", err, d)
	if err == nil {
		t.Fatal("an oversized reply was accepted")
	}
	if d > 4*time.Second {
		t.Errorf("an oversized reply took %v to report — the caller waited out its own deadline for a body the transport had already thrown away", d)
	}
}

// A response body may be a JSON array, and every element of it is handled
// separately. Each server-initiated request in that array was answered on a
// goroutine of its own, holding a ten-second context and posting back at the
// server: one reply of a few hundred kilobytes is tens of thousands of
// goroutines and tens of thousands of outbound requests, and over SSE the
// server never has to stop sending them.
func TestABatchOfServerRequestsDoesNotSpawnOneGoroutineEach(t *testing.T) {
	const n = 20000
	parts := make([]string, n)
	for i := range parts {
		parts[i] = fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"sampling/createMessage"}`, i+1)
	}
	batch := "[" + strings.Join(parts, ",") + "]"

	var posts int64
	srv := httptest.NewServer(mcpHandshake(func(w http.ResponseWriter, req rpcMsg) {
		if req.Method == "" { // one of our replies coming back
			atomic.AddInt64(&posts, 1)
			w.WriteHeader(http.StatusAccepted)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, batch)
	}))
	defer srv.Close()

	c, err := Start(context.Background(), config.MCPServerConfig{Name: "flood", URL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	before := runtime.NumGoroutine()
	peak := before
	done := make(chan struct{})
	go func() {
		for {
			select {
			case <-done:
				return
			default:
			}
			if g := runtime.NumGoroutine(); g > peak {
				peak = g
			}
			time.Sleep(time.Millisecond)
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, _, _ = c.CallTool(ctx, "anything", nil)
	time.Sleep(300 * time.Millisecond)
	close(done)

	t.Logf("goroutines: %d before, %d peak, for a batch of %d server requests", before, peak, n)
	if grew := peak - before; grew > 500 {
		t.Errorf("one reply of %d server-initiated requests spawned %d goroutines", n, grew)
	}
}
