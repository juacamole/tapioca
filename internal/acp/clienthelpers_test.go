package acp

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"sync"
	"testing"
	"time"

	"tapioca/internal/agent"
	"tapioca/internal/config"
	"tapioca/internal/provider"
	"tapioca/internal/tools"
)

// fakeAgent is the other end of a client connection: an ACP agent scripted by
// the test. It reuses the connection this package already has, so the tests
// exercise the real framing rather than a second implementation of it.
type fakeAgent struct {
	conn *conn
	sc   *bufio.Scanner
	turn func(f *fakeAgent) string // one prompt turn; returns the stop reason

	mu        sync.Mutex
	chose     string // the option the client picked, per request
	cancelled bool
}

func (f *fakeAgent) run() {
	for f.sc.Scan() {
		var msg rpcMsg
		if json.Unmarshal(f.sc.Bytes(), &msg) != nil {
			continue
		}
		if msg.Method == "" {
			m := msg
			f.conn.routeResponse(&m)
			continue
		}
		switch msg.Method {
		case "initialize":
			f.conn.respond(msg.ID, map[string]any{"protocolVersion": protocolVersion})
		case "session/new":
			f.conn.respond(msg.ID, map[string]any{"sessionId": "sess-1"})
		case "session/prompt":
			// The turn runs off the read loop: it may ask for permission, and
			// the answer arrives on this same connection.
			id := msg.ID
			go func() {
				f.conn.respond(id, map[string]any{"stopReason": f.turn(f)})
			}()
		case "session/cancel":
			f.mu.Lock()
			f.cancelled = true
			f.mu.Unlock()
		default:
			if msg.ID != nil {
				f.conn.fail(msg.ID, -32601, "no")
			}
		}
	}
}

func (f *fakeAgent) update(u map[string]any) {
	_ = f.conn.notify("session/update", map[string]any{"sessionId": "sess-1", "update": u})
}

// ask sends a permission request and records what the client answered.
func (f *fakeAgent) ask(call map[string]any) string {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	res, err := f.conn.call(ctx, "session/request_permission", map[string]any{
		"sessionId": "sess-1",
		"toolCall":  call,
		"options": []any{
			map[string]any{"optionId": "yes", "name": "Allow", "kind": "allow_once"},
			map[string]any{"optionId": "yes-always", "name": "Always", "kind": "allow_always"},
			map[string]any{"optionId": "no", "name": "Reject", "kind": "reject_once"},
		},
	})
	outcome := "error"
	if err == nil {
		var out struct {
			Outcome struct {
				Outcome  string `json:"outcome"`
				OptionID string `json:"optionId"`
			} `json:"outcome"`
		}
		if json.Unmarshal(res, &out) == nil {
			outcome = out.Outcome.OptionID
			if out.Outcome.Outcome != "selected" {
				outcome = out.Outcome.Outcome
			}
		}
	}
	f.mu.Lock()
	f.chose = outcome
	f.mu.Unlock()
	return outcome
}

func (f *fakeAgent) chosen() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.chose
}

func (f *fakeAgent) sawCancel() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.cancelled
}

// connectFake wires a client to a scripted agent over pipes and gives it a
// tab, the way the frontend does after a successful dial.
func connectFake(t *testing.T, gate *tools.Executor, turn func(f *fakeAgent) string) (*agent.Agent, *fakeAgent) {
	t.Helper()
	clientIn, agentOut := io.Pipe()
	agentIn, clientOut := io.Pipe()

	sc := bufio.NewScanner(agentIn)
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	f := &fakeAgent{conn: newConn(agentOut), sc: sc, turn: turn}
	done := make(chan struct{})
	go func() {
		defer close(done)
		f.run()
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	c, err := Connect(ctx, "ext", clientIn, clientOut, t.TempDir(), gate)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	mgr := agent.NewManager(config.Default(), nil, gate)
	a := mgr.NewExternal("ext", c)
	c.Attach(a)
	t.Cleanup(func() {
		c.Close()
		agentOut.Close()
		<-done
	})
	return a, f
}

// prompt runs one turn and collects its events, answering permission prompts
// the way the frontend would. Answers are used in order; anything unanswered
// counts as a refusal, which is what an unattended prompt means.
func prompt(t *testing.T, a *agent.Agent, text string, answers ...tools.Decision) []agent.Event {
	t.Helper()
	a.Messages = append(a.Messages, provider.TextMessage("user", text))
	a.Send(append([]provider.Message(nil), a.Messages...))
	var evs []agent.Event
	deadline := time.After(20 * time.Second)
	for {
		select {
		case ev := <-a.Events:
			evs = append(evs, ev)
			if ev.Kind == agent.EvPermission && ev.Perm != nil {
				d := tools.Decision{}
				if len(answers) > 0 {
					d, answers = answers[0], answers[1:]
				}
				ev.Perm.Reply <- d
			}
			if ev.Kind == agent.EvDone {
				return evs
			}
		case <-deadline:
			t.Fatal("timed out waiting for the turn to end")
		}
	}
}

// permissions returns the prompts a turn put in front of the user.
func permissions(evs []agent.Event) []*agent.PermissionReq {
	var out []*agent.PermissionReq
	for _, ev := range evs {
		if ev.Kind == agent.EvPermission && ev.Perm != nil {
			out = append(out, ev.Perm)
		}
	}
	return out
}

func messages(evs []agent.Event) []provider.Message {
	var out []provider.Message
	for _, ev := range evs {
		if ev.Kind == agent.EvMessage && ev.Message != nil {
			out = append(out, *ev.Message)
		}
	}
	return out
}

func streamed(evs []agent.Event) string {
	s := ""
	for _, ev := range evs {
		if ev.Kind == agent.EvTextDelta {
			s += ev.Text
		}
	}
	return s
}

func errorsOf(evs []agent.Event) []string {
	var out []string
	for _, ev := range evs {
		switch ev.Kind {
		case agent.EvError:
			if ev.Err != nil {
				out = append(out, ev.Err.Error())
			}
		case agent.EvNotice:
			out = append(out, ev.Text)
		}
	}
	return out
}

// bashCall is what an agent sends before running a shell command.
func bashCall(command string) map[string]any {
	return map[string]any{
		"toolCallId": "call-1",
		"title":      "Run " + command,
		"kind":       "execute",
		"status":     "pending",
		"rawInput":   map[string]any{"command": command},
	}
}
