package ui

import (
	"testing"
	"time"

	"tapioca/internal/agent"
	"tapioca/internal/config"
	"tapioca/internal/provider"
)

func thinkingApp(verbose bool) *App {
	return &App{
		cfg:       &config.Config{Verbose: verbose},
		thinkOpen: map[string]bool{},
		thinkAt:   map[int]string{},
	}
}

func TestThinkingOpenFollowsVerboseUntilClicked(t *testing.T) {
	m := thinkingApp(false)
	if m.thinkingOpen("k") {
		t.Error("thoughts should start collapsed")
	}
	m.thinkOpen["k"] = true
	if !m.thinkingOpen("k") {
		t.Error("an explicit expand was ignored")
	}

	v := thinkingApp(true)
	if !v.thinkingOpen("k") {
		t.Error("verbose mode should expand by default")
	}
	// An explicit collapse must win over verbose, or clicking does nothing
	// for anyone running verbose.
	v.thinkOpen["k"] = false
	if v.thinkingOpen("k") {
		t.Error("explicit collapse was overridden by verbose")
	}
}

func TestThinkKeyIsStablePerBlock(t *testing.T) {
	now := time.Now()
	a := &provider.Message{Time: now}
	b := &provider.Message{Time: now.Add(time.Second)}
	if thinkKey(a, 0) == thinkKey(a, 1) {
		t.Error("blocks in one message share a key")
	}
	if thinkKey(a, 0) == thinkKey(b, 0) {
		t.Error("different messages share a key")
	}
	if thinkKey(a, 0) != thinkKey(&provider.Message{Time: now}, 0) {
		t.Error("key is not stable across re-renders")
	}
}

func TestIsThinkHeader(t *testing.T) {
	SetGlyphs("unicode")
	defer SetGlyphs(defaultGlyphs)
	for _, s := range []string{
		thinkHeader("some text", false),
		thinkHeader("some text", true),
		"  " + thinkHeader("indented", false),
	} {
		if !isThinkHeader(s) {
			t.Errorf("header not recognized: %q", s)
		}
	}
	for _, s := range []string{"answer number 1", "", "thought about it", "› thoughts of mine"} {
		if isThinkHeader(s) {
			t.Errorf("ordinary text taken for a header: %q", s)
		}
	}
}

func TestMapThinkLinesPairsHeadersInOrder(t *testing.T) {
	SetGlyphs("unicode")
	defer SetGlyphs(defaultGlyphs)
	m := thinkingApp(false)
	t0 := time.Now()
	msgs := []provider.Message{
		{Role: "user", Time: t0, Blocks: []provider.Block{{Type: "text", Text: "q"}}},
		{Role: "assistant", Time: t0.Add(time.Second), Blocks: []provider.Block{
			{Type: "thinking", Text: "first"},
			{Type: "text", Text: "a"},
		}},
		{Role: "assistant", Time: t0.Add(2 * time.Second), Blocks: []provider.Block{
			{Type: "thinking", Text: "second"},
		}},
	}
	a := &agent.Agent{Messages: msgs}
	m.chatPlain = []string{
		"| you",
		"q",
		"| agent-1",
		thinkHeader("first", false),
		"a",
		"| agent-1",
		thinkHeader("second", true),
	}
	m.mapThinkLines(a)

	if got, want := m.thinkAt[3], thinkKey(&msgs[1], 0); got != want {
		t.Errorf("line 3 mapped to %q, want %q", got, want)
	}
	if got, want := m.thinkAt[6], thinkKey(&msgs[2], 0); got != want {
		t.Errorf("line 6 mapped to %q, want %q", got, want)
	}
	if _, ok := m.thinkAt[4]; ok {
		t.Error("a non-header line was mapped")
	}
}

// The in-flight block has no message yet, so its header must map to nothing
// rather than to the last finished thought.
func TestStreamingHeaderIsNotMapped(t *testing.T) {
	SetGlyphs("unicode")
	defer SetGlyphs(defaultGlyphs)
	m := thinkingApp(false)
	msg := provider.Message{Role: "assistant", Time: time.Now(),
		Blocks: []provider.Block{{Type: "thinking", Text: "done"}}}
	a := &agent.Agent{Messages: []provider.Message{msg}}
	m.chatPlain = []string{
		thinkHeader("done", false),
		thinkHeader("in flight", false),
	}
	m.mapThinkLines(a)
	if len(m.thinkAt) != 1 {
		t.Fatalf("mapped %d headers, want 1: %+v", len(m.thinkAt), m.thinkAt)
	}
	if _, ok := m.thinkAt[1]; ok {
		t.Error("the streaming header was mapped to a finished block")
	}
}
