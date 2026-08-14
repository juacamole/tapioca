package acp

import (
	"bufio"
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"tapioca/internal/agent"
	"tapioca/internal/config"
	"tapioca/internal/tools"
)

// floodEnv turns the helper into an agent that answers the handshake and then
// writes one line too long to read, staying alive afterwards.
const floodEnv = "TAPIOCA_ACP_FLOOD"

func TestFloodingAgentProcess(t *testing.T) {
	if os.Getenv(floodEnv) != "1" {
		t.Skip("helper for TestALineTooLongEndsTheTabInsteadOfHangingIt")
	}
	sc := bufio.NewScanner(os.Stdin)
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	f := &fakeAgent{conn: newConn(os.Stdout), sc: sc}
	f.turn = func(f *fakeAgent) string {
		// No newline, ever. The parent reads until its buffer is exceeded.
		chunk := strings.Repeat("x", 4096)
		for range 64 {
			_, _ = os.Stdout.WriteString(chunk)
		}
		// Still running, still holding stdout open: this is the state that used
		// to wedge the parent's cmd.Wait.
		time.Sleep(60 * time.Second)
		return "end_turn"
	}
	f.run()
}

// A line longer than the buffer stops the read loop while the agent is still
// running. Nothing drains its stdout after that, so it blocks on a full pipe —
// and waiting for it to exit blocks on that, which left the tab with no answer,
// no error and no way back. The turn has to end and the tab has to be told.
func TestALineTooLongEndsTheTabInsteadOfHangingIt(t *testing.T) {
	old := maxLineBytes
	maxLineBytes = 128 << 10
	t.Cleanup(func() { maxLineBytes = old })

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	gate := gateFor(t, tools.ModeBypass)
	c, err := Dial(ctx, config.ExternalAgent{
		Name:    "flooder",
		Command: os.Args[0],
		Args:    []string{"-test.run=TestFloodingAgentProcess"},
		Env:     map[string]string{floodEnv: "1"},
	}, t.TempDir(), gate)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(c.Close)

	mgr := agent.NewManager(config.Default(), nil, gate)
	a := mgr.NewExternal("flooder", c)
	c.Attach(a)

	done := make(chan []agent.Event, 1)
	go func() { done <- prompt(t, a, "go") }()
	select {
	case evs := <-done:
		var text []string
		for _, ev := range evs {
			text = append(text, ev.Text)
		}
		joined := strings.Join(text, " ")
		if !strings.Contains(joined, "too long") && !strings.Contains(joined, "flooder") {
			t.Errorf("the tab was not told why it stopped: %q", joined)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("the turn never ended: the read loop gave up and the wait for the agent blocked on a pipe nobody was draining")
	}
}
