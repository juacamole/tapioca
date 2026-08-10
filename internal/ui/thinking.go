package ui

import (
	"fmt"
	"strings"

	"tapioca/internal/agent"
	"tapioca/internal/provider"
)

// Thinking blocks collapse to one line so a long chain of thought does not
// bury the answer, but the reasoning is often the interesting part — clicking
// the line expands that block alone.

const (
	thinkCollapsedWord = "thought"
	thinkExpandedWord  = "thinking"
)

// thinkKey identifies a thinking block across re-renders. The message time
// rather than its index, so compaction cannot point a key at another block.
func thinkKey(m *provider.Message, blockIdx int) string {
	return fmt.Sprintf("%d:%d", m.Time.UnixNano(), blockIdx)
}

// thinkingOpen reports whether a block renders expanded. An explicit click
// wins; otherwise the verbose setting decides.
func (m *App) thinkingOpen(key string) bool {
	if open, ok := m.thinkOpen[key]; ok {
		return open
	}
	return m.cfg.Verbose
}

// thinkHeader is the clickable first line of a thinking block.
func thinkHeader(text string, open bool) string {
	word, marker := thinkCollapsedWord, gl.caret
	if open {
		word, marker = thinkExpandedWord, gl.todoDoing
	}
	return fmt.Sprintf("%s %s (%s)", marker, word, humanTokens(len(text)))
}

// isThinkHeader recognizes a rendered header line, so clicks can be mapped
// back to the block that produced it.
func isThinkHeader(plain string) bool {
	s := strings.TrimSpace(plain)
	for _, marker := range []string{gl.caret, gl.todoDoing} {
		for _, word := range []string{thinkCollapsedWord, thinkExpandedWord} {
			if strings.HasPrefix(s, marker+" "+word+" (") {
				return true
			}
		}
	}
	return false
}

// mapThinkLines pairs each rendered header line with its block, in document
// order. Both states render exactly one header, so the Nth header line is the
// Nth thinking block.
func (m *App) mapThinkLines(a *agent.Agent) {
	m.thinkAt = map[int]string{}
	if a == nil {
		return
	}
	var keys []string
	for i := range a.Messages {
		msg := &a.Messages[i]
		if msg.Hidden {
			continue
		}
		for bi, bl := range msg.Blocks {
			if bl.Type == "thinking" {
				keys = append(keys, thinkKey(msg, bi))
			}
		}
	}
	n := 0
	for line, plain := range m.plainLines() {
		if !isThinkHeader(plain) {
			continue
		}
		if n < len(keys) {
			m.thinkAt[line] = keys[n]
		}
		n++ // past the end is the in-flight block, which has no key yet
	}
}

// toggleThinkingAt expands or collapses the block whose header is on this
// line, reporting whether the line was a header at all. The line map is built
// here rather than on every refresh: only a click needs it.
func (m *App) toggleThinkingAt(line int) bool {
	if m.thinkAt == nil {
		m.mapThinkLines(m.mgr.ActiveAgent())
	}
	key, ok := m.thinkAt[line]
	if !ok {
		return false
	}
	m.thinkOpen[key] = !m.thinkingOpen(key)
	m.refreshChat(false)
	return true
}
