package ui

import (
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// Every failure in this app arrives as a flash: provider errors, MCP failures,
// tool errors, permission denials. A flash overwrites one string and a timer
// clears it, so a message you did not finish reading is gone and there is no
// record of it anywhere.
//
// That is the same defect as the /model dead end in a different place — the
// app knows something, says it once, and leaves nothing to act on. It bites
// hardest on the messages most worth reading, because the long ones (a
// provider's JSON error body, an MCP handshake failure) are exactly the ones
// three seconds is not enough for.
//
// Nothing here is written to disk. Provider errors quote request content, and
// a log file would mean deciding what to redact in a place nobody is looking.

// logMax bounds the history. Long enough to cover a working session, short
// enough that a server flooding errors cannot grow it without limit.
const logMax = 200

type logEntry struct {
	at    time.Time
	text  string
	isErr bool
}

// record appends a message to the history. It takes the already-sanitized text
// so a credential cannot enter the log by a route the flash itself would have
// scrubbed.
func (m *App) record(text string, isErr bool) {
	text = strings.TrimSpace(text)
	if text == "" {
		return
	}
	// A repeated message is counted rather than repeated: a retry loop would
	// otherwise push everything else out of a bounded history.
	if n := len(m.msgLog); n > 0 && m.msgLog[n-1].text == text && m.msgLog[n-1].isErr == isErr {
		m.msgLog[n-1].at = time.Now()
		return
	}
	m.msgLog = append(m.msgLog, logEntry{at: time.Now(), text: text, isErr: isErr})
	if len(m.msgLog) > logMax {
		m.msgLog = m.msgLog[len(m.msgLog)-logMax:]
	}
}

// logText renders the history newest last, so the most recent message is where
// the eye already is when the overlay opens at the bottom.
func (m *App) logText() string {
	if len(m.msgLog) == 0 {
		return "No messages yet."
	}
	var b strings.Builder
	for i, e := range m.msgLog {
		if i > 0 {
			b.WriteString("\n")
		}
		mark := " "
		if e.isErr {
			mark = gl.toolErr
		}
		b.WriteString(e.at.Format("15:04:05") + " " + mark + " " + e.text)
	}
	return b.String()
}

// cmdLog opens the history. The overlay wraps and scrolls, so a long provider
// error is readable in full rather than truncated to the status line.
func cmdLog(m *App, _ string) tea.Cmd {
	m.openTextOverlay("messages", m.logText())
	return nil
}
