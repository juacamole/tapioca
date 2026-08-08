package ui

import (
	"context"
	"errors"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"tapioca/internal/agent"
	"tapioca/internal/provider"
	"tapioca/internal/textenc"
)

const (
	compactKeepTurns  = 2
	compactToolTrunc  = 2000
	summaryMarker     = "[Conversation summary]"
	compactBufferBig  = 20_000
	compactPctTrigger = 80
)

// compactThreshold is the context level that triggers auto-compaction.
func compactThreshold(win int) int {
	if win > 200_000 {
		return win - compactBufferBig
	}
	return win * compactPctTrigger / 100
}

// splitForCompact separates the summarizable head from the last keepTurns
// real user turns, which are preserved verbatim. Splitting on a user prompt
// can never sever a tool_use/tool_result pair.
func splitForCompact(msgs []provider.Message, keepTurns int) (head, tail []provider.Message) {
	idx := len(msgs)
	turns := 0
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == "user" && !msgs[i].IsToolResult() && !msgs[i].Hidden {
			turns++
			idx = i
			if turns == keepTurns {
				break
			}
		}
	}
	if idx == 0 {
		return nil, msgs
	}
	return msgs[:idx], msgs[idx:]
}

func textSize(msgs []provider.Message) int {
	n := 0
	for _, m := range msgs {
		for _, b := range m.Blocks {
			n += len(b.Text) + len(b.Content)
		}
	}
	return n
}

const compactInstruction = "Summarize this conversation into a compact brief for continuing the work. " +
	"Sections: Objective; Important details (exact file paths, symbols, commands, error strings); " +
	"Work state (done / in progress / blocked); Next steps. Terse bullets. Reply with only the summary."

const compactAnchored = " A previous summary opens this conversation — merge it with the newer events " +
	"into one updated summary instead of starting over."

// compactCmd summarizes everything before the preserved tail. Returns nil
// when there is nothing worth compacting.
func (m *App) compactCmd(a *agent.Agent) tea.Cmd {
	head, tail := splitForCompact(a.Messages, compactKeepTurns)
	if len(head) == 0 {
		return nil
	}
	instruction := compactInstruction
	if len(head) > 0 && strings.HasPrefix(head[0].Text(), summaryMarker) {
		instruction += compactAnchored
	}

	reqMsgs := make([]provider.Message, 0, len(head)+1)
	for _, msg := range head {
		c := msg
		var blocks []provider.Block
		for _, b := range msg.Blocks {
			switch b.Type {
			case "thinking":
				continue
			case "tool_result":
				if len(b.Content) > compactToolTrunc {
					b.Content = textenc.Cut(b.Content, compactToolTrunc) + "\n[truncated]"
				}
			}
			blocks = append(blocks, b)
		}
		if len(blocks) == 0 {
			continue
		}
		c.Blocks = blocks
		reqMsgs = append(reqMsgs, c)
	}
	reqMsgs = append(reqMsgs, provider.TextMessage("user", instruction))

	p := a.Provider
	model := a.Model
	id := a.ID
	baseLen := len(a.Messages)
	headSize := textSize(head)
	tailCopy := make([]provider.Message, len(tail))
	copy(tailCopy, tail)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	m.compacting[id] = cancel

	return func() tea.Msg {
		if p == nil {
			return compactDoneMsg{agentID: id, err: errors.New("no provider configured")}
		}
		events := make(chan provider.Event, 64)
		go func() {
			for range events {
			}
		}()
		msg, err := p.Stream(ctx, provider.Request{
			Model:     model,
			System:    "You compress conversations for context handoff.",
			Messages:  reqMsgs,
			MaxTokens: 2048,
		}, events)
		if err != nil {
			return compactDoneMsg{agentID: id, err: err}
		}
		summary := strings.TrimSpace(msg.Text())
		if summary == "" {
			return compactDoneMsg{agentID: id, err: errors.New("summarizer returned nothing")}
		}
		if len(summary) >= headSize {
			return compactDoneMsg{agentID: id, err: errors.New("summary did not shrink the history")}
		}
		newMsgs := append([]provider.Message{
			provider.TextMessage("user", summaryMarker+"\n\n"+summary),
		}, tailCopy...)
		return compactDoneMsg{
			agentID: id,
			baseLen: baseLen,
			msgs:    newMsgs,
			ctxEst:  (len(summary) + textSize(tailCopy)) / 4,
		}
	}
}
