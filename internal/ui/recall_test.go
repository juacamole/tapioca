package ui

import (
	"github.com/charmbracelet/bubbles/textarea"

	"reflect"
	"tapioca/internal/config"
	"testing"
	"time"

	"tapioca/internal/agent"
	"tapioca/internal/provider"
)

func note(text string) provider.Message {
	return provider.Message{
		Role:   "note",
		Blocks: []provider.Block{{Type: "text", Text: text}},
		Time:   time.Now(),
	}
}

// Up-arrow recall walked user prompts only. Slash commands are stored as
// "note" messages, so every /model, /connect and /effort was skipped and a
// command typed slightly wrong had to be retyped in full.
func TestRecallIncludesSlashCommands(t *testing.T) {
	a := &agent.Agent{Messages: []provider.Message{
		provider.TextMessage("user", "first prompt"),
		note("/effort high"),
		provider.TextMessage("assistant", "a reply"),
		note("/model gpt-4o"),
		provider.TextMessage("user", "second prompt"),
	}}

	got := recallHistory(a)
	want := []string{"first prompt", "/effort high", "/model gpt-4o", "second prompt"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("recallHistory() = %#v, want %#v", got, want)
	}
}

// Order is what makes recall predictable: pressing up twice must reach the
// entry before last, whichever kind it is.
func TestRecallIsChronological(t *testing.T) {
	a := &agent.Agent{Messages: []provider.Message{
		note("/connect"),
		provider.TextMessage("user", "a prompt"),
	}}
	got := recallHistory(a)
	if len(got) != 2 || got[0] != "/connect" || got[1] != "a prompt" {
		t.Fatalf("recallHistory() = %#v, want the note first", got)
	}
}

// Assistant replies, tool results and hidden notices are not things anyone
// typed, so they must stay out of the cycle.
func TestRecallExcludesWhatWasNotTyped(t *testing.T) {
	hidden := provider.TextMessage("user", "files changed on disk")
	hidden.Hidden = true
	toolResult := provider.Message{
		Role:   "user",
		Blocks: []provider.Block{{Type: "tool_result", Content: "output"}},
	}
	a := &agent.Agent{Messages: []provider.Message{
		provider.TextMessage("assistant", "a reply"),
		hidden,
		toolResult,
		provider.TextMessage("user", "   "),
		provider.TextMessage("user", "the only one"),
	}}
	if got := recallHistory(a); len(got) != 1 || got[0] != "the only one" {
		t.Errorf("recallHistory() = %#v, want just the typed prompt", got)
	}
}

// Recalling a slash command must not open the completion popup: it takes over
// up and down, which strands you cycling command names instead of walking back
// through history. Typing brings completion back.
func TestRecallSuppressesSlashCompletion(t *testing.T) {
	m := &App{cfg: config.Default(), w: 100, h: 30, mgr: &agent.Manager{}}
	m.ta = textarea.New()
	m.ta.SetValue("/thinking")

	if len(m.slashMatches()) == 0 {
		t.Fatal("a typed /thinking should complete — the test is not exercising anything")
	}
	m.recalling = true
	if got := m.slashMatches(); len(got) != 0 {
		t.Errorf("recall offered %d completions; up/down would drive the popup, not history", len(got))
	}
	m.recalling = false
	if len(m.slashMatches()) == 0 {
		t.Error("completion did not come back once recall ended")
	}
}
