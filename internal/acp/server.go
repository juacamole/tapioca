package acp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"

	"tapioca/internal/agent"
	"tapioca/internal/config"
	"tapioca/internal/mcp"
	"tapioca/internal/provider"
	"tapioca/internal/session"
	"tapioca/internal/tools"
)

// Server serves one ACP connection. Each session/new gets its own agent,
// executor and working directory, so an editor can drive several projects.
type Server struct {
	cfg  *config.Config
	conn *conn

	mu       sync.Mutex
	sessions map[string]*acpSession
}

type acpSession struct {
	id    string
	agent *agent.Agent
	exec  *tools.Executor
	reg   *mcp.Registry

	cancelMu sync.Mutex
	cancel   context.CancelFunc
}

// Serve runs the protocol loop until the input closes.
func Serve(cfg *config.Config, in io.Reader, out io.Writer) error {
	s := &Server{cfg: cfg, conn: newConn(out), sessions: map[string]*acpSession{}}
	sc := bufio.NewScanner(in)
	sc.Buffer(make([]byte, 0, 64*1024), 32*1024*1024)
	var wg sync.WaitGroup
	for sc.Scan() {
		line := sc.Bytes()
		if len(strings.TrimSpace(string(line))) == 0 {
			continue
		}
		var msg rpcMsg
		if err := json.Unmarshal(line, &msg); err != nil {
			continue
		}
		if msg.Method == "" { // a reply to something we asked
			m := msg
			s.conn.routeResponse(&m)
			continue
		}
		// Handlers run concurrently: a prompt turn lasts minutes and must not
		// block session/cancel arriving behind it.
		m := msg
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.dispatch(&m)
		}()
	}
	wg.Wait()
	s.closeAll()
	return sc.Err()
}

func (s *Server) closeAll() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, sess := range s.sessions {
		if sess.reg != nil {
			sess.reg.CloseAll()
		}
		if sess.exec != nil {
			sess.exec.StopJobs()
		}
	}
}

func (s *Server) dispatch(msg *rpcMsg) {
	switch msg.Method {
	case "initialize":
		s.handleInitialize(msg)
	case "authenticate":
		s.conn.respond(msg.ID, map[string]any{})
	case "session/new":
		s.handleNewSession(msg)
	case "session/prompt":
		s.handlePrompt(msg)
	case "session/cancel":
		s.handleCancel(msg)
	default:
		if msg.ID != nil {
			s.conn.fail(msg.ID, -32601, "method not supported: "+msg.Method)
		}
	}
}

func (s *Server) handleInitialize(msg *rpcMsg) {
	s.conn.respond(msg.ID, map[string]any{
		"protocolVersion": protocolVersion,
		"agentCapabilities": map[string]any{
			"loadSession": false,
			"promptCapabilities": map[string]any{
				"image":           false,
				"audio":           false,
				"embeddedContext": true,
			},
		},
		"agentInfo": map[string]any{
			"name":    "tapioca",
			"title":   "Tapioca",
			"version": "0.1.0",
		},
		"authMethods": []any{}, // configured locally; nothing to log into
	})
}

func (s *Server) handleNewSession(msg *rpcMsg) {
	var p struct {
		Cwd        string                   `json:"cwd"`
		MCPServers []config.MCPServerConfig `json:"mcpServers"`
	}
	_ = json.Unmarshal(msg.Params, &p)
	cwd := p.Cwd
	if strings.TrimSpace(cwd) == "" {
		cwd, _ = os.Getwd()
	}
	if info, err := os.Stat(cwd); err != nil || !info.IsDir() {
		s.conn.fail(msg.ID, -32602, fmt.Sprintf("cwd %q is not a directory", p.Cwd))
		return
	}

	exec := tools.NewExecutor(cwd, s.cfg.PermissionMode)
	exec.SetBashPrefixes(s.cfg.BashAllow)
	exec.SetRules(s.cfg.Permissions.Allow, s.cfg.Permissions.Ask, s.cfg.Permissions.Deny)
	exec.SetSandbox(s.cfg.Sandbox)
	exec.SetSandboxNetwork(s.cfg.SandboxNetwork)

	reg := mcp.NewRegistry()
	servers := s.cfg.MCP
	if len(p.MCPServers) > 0 {
		// The editor's list replaces ours: it knows what this project needs.
		servers = p.MCPServers
	}
	for _, mc := range servers {
		client, err := mcp.Start(context.Background(), mc)
		if err != nil {
			reg.SetError(mc.Name, err.Error())
			continue
		}
		reg.Add(client)
	}

	mgr := agent.NewManager(s.cfg, reg, exec)
	a := mgr.NewAgent()
	if a.ProviderErr != "" {
		reg.CloseAll()
		s.conn.fail(msg.ID, -32603, a.ProviderErr)
		return
	}
	if a.Model == "" {
		reg.CloseAll()
		s.conn.fail(msg.ID, -32603, "no model configured — set default_model in config.toml")
		return
	}

	sess := &acpSession{id: session.NewID(), agent: a, exec: exec, reg: reg}
	s.mu.Lock()
	s.sessions[sess.id] = sess
	s.mu.Unlock()
	s.conn.respond(msg.ID, map[string]any{"sessionId": sess.id})
}

func (s *Server) session(id string) *acpSession {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sessions[id]
}

func (s *Server) handleCancel(msg *rpcMsg) {
	var p struct {
		SessionID string `json:"sessionId"`
	}
	_ = json.Unmarshal(msg.Params, &p)
	if sess := s.session(p.SessionID); sess != nil {
		sess.cancelMu.Lock()
		if sess.cancel != nil {
			sess.cancel()
		}
		sess.cancelMu.Unlock()
	}
}

// promptText flattens ACP content blocks into the text the model receives.
func promptText(blocks []json.RawMessage) string {
	var parts []string
	for _, raw := range blocks {
		var b struct {
			Type     string `json:"type"`
			Text     string `json:"text"`
			Resource struct {
				URI  string `json:"uri"`
				Text string `json:"text"`
			} `json:"resource"`
			ResourceLink struct {
				URI string `json:"uri"`
			} `json:"resourceLink"`
		}
		if json.Unmarshal(raw, &b) != nil {
			continue
		}
		switch b.Type {
		case "text":
			parts = append(parts, b.Text)
		case "resource":
			// Editors inline the file they want discussed; keep the path so
			// the model can reason about (and re-read) it.
			if b.Resource.Text != "" {
				parts = append(parts, fmt.Sprintf("%s:\n%s", b.Resource.URI, b.Resource.Text))
			} else if b.Resource.URI != "" {
				parts = append(parts, b.Resource.URI)
			}
		case "resource_link":
			if b.ResourceLink.URI != "" {
				parts = append(parts, b.ResourceLink.URI)
			}
		}
	}
	return strings.TrimSpace(strings.Join(parts, "\n\n"))
}

func (s *Server) handlePrompt(msg *rpcMsg) {
	var p struct {
		SessionID string            `json:"sessionId"`
		Prompt    []json.RawMessage `json:"prompt"`
	}
	if err := json.Unmarshal(msg.Params, &p); err != nil {
		s.conn.fail(msg.ID, -32602, err.Error())
		return
	}
	sess := s.session(p.SessionID)
	if sess == nil {
		s.conn.fail(msg.ID, -32602, "unknown sessionId "+p.SessionID)
		return
	}
	text := promptText(p.Prompt)
	if text == "" {
		s.conn.fail(msg.ID, -32602, "empty prompt")
		return
	}

	ctx, cancel := context.WithCancel(context.Background())
	sess.cancelMu.Lock()
	sess.cancel = cancel
	sess.cancelMu.Unlock()
	defer cancel()

	a := sess.agent
	a.Messages = append(a.Messages, provider.TextMessage("user", text))
	history := make([]provider.Message, len(a.Messages))
	copy(history, a.Messages)
	a.Send(history)

	stop := s.pump(ctx, sess)
	s.conn.respond(msg.ID, map[string]any{"stopReason": stop})
}
