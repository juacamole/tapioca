package ui

import (
	"strings"
	"testing"

	"tapioca/internal/config"
)

// The report: "you see the error message for like 3 seconds, then its gone and
// i get no info where it is." A message that has scrolled past must still be
// readable afterwards.
func TestMessagesSurviveBeingReplaced(t *testing.T) {
	m := &App{cfg: config.Default()}
	m.setFlash("first thing that went wrong", true)
	m.setFlash("second thing", true)
	m.setFlash("something fine", false)

	got := m.logText()
	for _, want := range []string{"first thing that went wrong", "second thing", "something fine"} {
		if !strings.Contains(got, want) {
			t.Errorf("the log lost %q", want)
		}
	}
}

// Errors are the messages most worth reading and the least likely to be read
// in four seconds, and there was no way to ask for them back.
func TestErrorsDoNotExpireButNoticesDo(t *testing.T) {
	m := &App{cfg: config.Default()}

	m.setFlash("a provider error", true)
	m.Update(flashClearMsg{seq: m.flashSeq})
	if m.flash == "" {
		t.Error("an error expired on its timer")
	}

	m.setFlash("saved", false)
	m.Update(flashClearMsg{seq: m.flashSeq})
	if m.flash != "" {
		t.Errorf("an informational flash stayed: %q", m.flash)
	}
}

// A retry loop would otherwise push everything else out of a bounded history.
func TestRepeatsDoNotFloodTheHistory(t *testing.T) {
	m := &App{cfg: config.Default()}
	for i := 0; i < 50; i++ {
		m.setFlash("connection refused", true)
	}
	if len(m.msgLog) != 1 {
		t.Errorf("50 identical messages produced %d entries, want 1", len(m.msgLog))
	}
	m.setFlash("something else", true)
	if len(m.msgLog) != 2 {
		t.Errorf("a different message did not append: %d entries", len(m.msgLog))
	}
}

// A server flooding distinct errors must not grow the history without bound.
func TestHistoryIsBounded(t *testing.T) {
	m := &App{cfg: config.Default()}
	for i := 0; i < logMax*2; i++ {
		m.setFlash(strings.Repeat("x", i%40+1), true)
	}
	if len(m.msgLog) > logMax {
		t.Errorf("history grew to %d, cap is %d", len(m.msgLog), logMax)
	}
}

// The history is a second place messages live, so it must not be a second way
// for a terminal escape to reach the screen. Recording happens inside setFlash
// after sanitising, so this asserts that ordering has not been undone.
func TestHistoryIsSanitised(t *testing.T) {
	m := &App{cfg: config.Default()}
	m.setFlash("bad \x1b[2Jclear \x1b]0;title\x07thing", true)

	got := m.logText()
	if strings.ContainsAny(got, "\x1b\x07") {
		t.Errorf("an escape sequence reached the history: %q", got)
	}
	if !strings.Contains(got, "clear") {
		t.Errorf("sanitising ate the message: %q", got)
	}
}

// Newlines in a recorded message would let one entry draw as several and forge
// timestamps for messages that never happened.
func TestHistoryEntriesStayOnOneLine(t *testing.T) {
	m := &App{cfg: config.Default()}
	m.setFlash("first line\n10:00:00 x forged second line", true)
	if n := strings.Count(m.logText(), "\n"); n != 0 {
		t.Errorf("one message rendered as %d lines", n+1)
	}
}

func TestEmptyLogSaysSo(t *testing.T) {
	m := &App{cfg: config.Default()}
	if got := m.logText(); !strings.Contains(got, "No messages") {
		t.Errorf("logText() = %q on an empty history", got)
	}
}
