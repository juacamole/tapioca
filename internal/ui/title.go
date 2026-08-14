package ui

import (
	"context"
	"regexp"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

	"tapioca/internal/provider"
)

const (
	titleMaxLen    = 48
	titleMaxTokens = 40
	titleTimeout   = 20 * time.Second
)

// sessID tags the result: /new or /resume mid-generation must not have the
// previous session's title land on the current one.
type titleDoneMsg struct {
	sessID string
	name   string
}

const titleInstruction = "Write a title for a coding session that begins with the request below. " +
	"At most six words, no quotes, no trailing punctuation, no preamble. Reply with the title alone.\n\nRequest:\n"

// thinkBlock strips reasoning models' visible scratchpad; several local models
// emit it even with thinking disabled, and it would become the title.
var thinkBlock = regexp.MustCompile(`(?is)<(think|thinking)>.*?</(think|thinking)>`)

// titleCmd asks a model for a short session title. The caller has already set
// the truncated fallback, so failure just leaves that in place.
func (m *App) titleCmd(prompt string) tea.Cmd {
	a := m.mgr.ActiveAgent()
	if a == nil || a.Provider == nil || a.Model == "" {
		return nil
	}
	p, model := a.Provider, a.Model
	// An explicit title_model keeps titling off an expensive main model.
	if tm := strings.TrimSpace(m.cfg.TitleModel); tm != "" {
		name, rest := m.cfg.DefaultProvider, tm
		if pn, mn, ok := strings.Cut(tm, ":"); ok && mn != "" {
			if _, exists := m.cfg.Providers[pn]; exists {
				name, rest = pn, mn
			}
		}
		if tp, err := m.mgr.ProviderFor(name); err == nil {
			p, model = tp, rest
		}
	}
	prompt = truncate(prompt, 2000)
	sessID := m.sessID

	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), titleTimeout)
		defer cancel()
		events := make(chan provider.Event, 16)
		go func() {
			for range events {
			}
		}()
		msg, err := p.Stream(ctx, provider.Request{
			Model:     model,
			System:    "You write terse session titles.",
			Messages:  []provider.Message{provider.TextMessage("user", titleInstruction+prompt)},
			MaxTokens: titleMaxTokens,
		}, events)
		if err != nil {
			return titleDoneMsg{sessID: sessID}
		}
		return titleDoneMsg{sessID: sessID, name: cleanTitle(msg.Text())}
	}
}

// cleanTitle reduces a model's reply to a usable title, or "" when the reply
// does not look like one.
func cleanTitle(s string) string {
	// The title is model-authored and ends up in the window title, the session
	// panel, the picker and --list-sessions, so it is stripped here rather than
	// at each of those.
	s = sanitizeText(thinkBlock.ReplaceAllString(s, ""))
	// Models that narrate ("Title: x", "Sure! ...") put the real answer last.
	lines := strings.Split(strings.TrimSpace(s), "\n")
	best := ""
	for _, l := range lines {
		l = strings.TrimSpace(l)
		if l == "" || strings.HasPrefix(l, "<") {
			continue
		}
		if _, after, ok := strings.Cut(l, "Title:"); ok {
			l = strings.TrimSpace(after)
		}
		best = l
		break
	}
	best = strings.Trim(best, " \t\"'`*#.,:;-")
	best = strings.Join(strings.Fields(best), " ")
	if best == "" || len(best) > 120 {
		return ""
	}
	return truncate(best, titleMaxLen)
}
