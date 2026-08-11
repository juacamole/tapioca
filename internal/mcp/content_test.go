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

// contentServer offers prompts and resources, and records what was asked for.
type contentServer struct {
	caps string

	mu     sync.Mutex
	called []string
	args   map[string]string
}

func (s *contentServer) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req rpcMsg
		if json.NewDecoder(r.Body).Decode(&req) != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		s.mu.Lock()
		s.called = append(s.called, req.Method)
		s.mu.Unlock()
		if req.ID == nil {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		var result string
		var rpcErr string
		switch req.Method {
		case "initialize":
			result = fmt.Sprintf(`{"protocolVersion":%q,"capabilities":%s}`, protocolVersion, s.caps)
		case "tools/list":
			result = `{"tools":[]}`
		case "prompts/list":
			result = `{"prompts":[{"name":"review","description":"review a diff",` +
				`"arguments":[{"name":"path","required":true}]}]}`
		case "prompts/get":
			var p struct {
				Name      string            `json:"name"`
				Arguments map[string]string `json:"arguments"`
			}
			json.Unmarshal(req.Params, &p)
			s.mu.Lock()
			s.args = p.Arguments
			s.mu.Unlock()
			result = `{"messages":[{"role":"user","content":{"type":"text","text":"first"}},` +
				`{"role":"user","content":{"type":"text","text":"second"}}]}`
		case "resources/list":
			result = `{"resources":[{"uri":"db://users","name":"users","mimeType":"text/plain"}]}`
		case "resources/read":
			result = `{"contents":[{"uri":"db://users","mimeType":"text/plain","text":"alice"},` +
				`{"uri":"db://users.png","mimeType":"image/png","blob":"AAAA"}]}`
		default:
			rpcErr = "method not found"
		}
		w.Header().Set("Content-Type", "application/json")
		if rpcErr != "" {
			fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"error":{"code":-32601,"message":%q}}`, req.ID, rpcErr)
			return
		}
		fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"result":%s}`, req.ID, result)
	})
}

func startContent(t *testing.T, caps string) (*Client, *contentServer) {
	t.Helper()
	s := &contentServer{caps: caps}
	srv := httptest.NewServer(s.handler())
	t.Cleanup(srv.Close)
	c, err := Start(context.Background(), config.MCPServerConfig{Name: "docs", URL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Close)
	return c, s
}

func TestPromptsAndResourcesAreListed(t *testing.T) {
	c, _ := startContent(t, `{"tools":{},"prompts":{},"resources":{}}`)
	prompts := c.Prompts()
	if len(prompts) != 1 || prompts[0].Name != "review" || prompts[0].Server != "docs" {
		t.Fatalf("prompts: %+v", prompts)
	}
	if len(prompts[0].Arguments) != 1 || !prompts[0].Arguments[0].Required {
		t.Fatalf("prompt arguments: %+v", prompts[0].Arguments)
	}
	res := c.Resources()
	if len(res) != 1 || res[0].URI != "db://users" {
		t.Fatalf("resources: %+v", res)
	}
}

// A server offering neither answers "method not found", not an empty list, so
// asking anyway would log an error on every connection.
func TestUnadvertisedCapabilitiesAreNotRequested(t *testing.T) {
	c, s := startContent(t, `{"tools":{}}`)
	s.mu.Lock()
	called := strings.Join(s.called, ",")
	s.mu.Unlock()
	if strings.Contains(called, "prompts/list") || strings.Contains(called, "resources/list") {
		t.Fatalf("asked for what the server never advertised: %s", called)
	}
	if len(c.Prompts()) != 0 || len(c.Resources()) != 0 {
		t.Fatal("invented prompts or resources")
	}
}

func TestGetPromptJoinsMessages(t *testing.T) {
	c, s := startContent(t, `{"prompts":{}}`)
	text, err := c.GetPrompt(context.Background(), "review", map[string]string{"path": "main.go"})
	if err != nil {
		t.Fatal(err)
	}
	if text != "first\n\nsecond" {
		t.Fatalf("got %q", text)
	}
	s.mu.Lock()
	args := s.args
	s.mu.Unlock()
	if args["path"] != "main.go" {
		t.Fatalf("arguments not passed: %+v", args)
	}
}

// Base64 in the transcript is tokens spent on nothing the model can read.
func TestReadResourceSkipsBinaryContents(t *testing.T) {
	c, _ := startContent(t, `{"resources":{}}`)
	text, err := c.ReadResource(context.Background(), "db://users")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(text, "alice") {
		t.Fatalf("text content missing: %q", text)
	}
	if strings.Contains(text, "AAAA") {
		t.Fatalf("base64 blob inlined: %q", text)
	}
	if !strings.Contains(text, "binary content omitted") {
		t.Fatalf("binary part not reported: %q", text)
	}
}

func TestRegistryNamespacesPromptsAndResources(t *testing.T) {
	c, _ := startContent(t, `{"prompts":{},"resources":{}}`)
	r := NewRegistry()
	r.Add(c)
	if got := r.AllPrompts(); len(got) != 1 || got[0].Server != "docs" {
		t.Fatalf("prompts: %+v", got)
	}
	if _, err := r.GetPrompt(context.Background(), "docs", "review", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := r.GetPrompt(context.Background(), "nope", "review", nil); err == nil {
		t.Fatal("resolved a prompt on a server that does not exist")
	}
	if text, err := r.ReadResource(context.Background(), "docs", "db://users"); err != nil ||
		!strings.Contains(text, "alice") {
		t.Fatalf("ReadResource = %q, %v", text, err)
	}
}
