package provider

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"
)

// maxResponseBytes caps one streamed response. Nothing else did: the
// scanner's line cap bounds a single line, not the answer assembled from
// thousands of them, and the turn carries no deadline — so a provider that
// keeps sending deltas, broken or hostile, grew the heap by gigabytes in
// seconds until the process died.
const maxResponseBytes = 32 << 20

func overLimit(n int) bool { return n > maxResponseBytes }

// maxStreamBlocks bounds the per-response maps keyed by a server-chosen index.
// Nothing legitimate opens thousands of content blocks or tool calls in one
// answer, and the index is whatever the server sends.
const maxStreamBlocks = 4096

// maxEventFrame bounds one AWS event-stream frame, whose length prefix is four
// bytes of unvalidated network input.
const maxEventFrame = 32 << 20

// streamAnthropicSSE turns an Anthropic-format event stream into events and
// a finished message. Bedrock and Vertex serve the same protocol behind
// different transports, so they feed their decoded stream through here
// rather than reimplementing the block assembly.
func (a *Anthropic) streamAnthropicSSE(ctx context.Context, model string, r io.Reader, out chan<- Event) (Message, error) {
	type blockBuilder struct {
		typ       string
		id, name  string
		data      string
		text      strings.Builder
		sig       strings.Builder
		inputJSON strings.Builder
	}
	builders := map[int]*blockBuilder{}
	var usage Usage
	var stopReason string
	msg := Message{Role: "assistant", Model: model, Time: time.Now()}

	finish := func() Message {
		idxs := make([]int, 0, len(builders))
		for i := range builders {
			idxs = append(idxs, i)
		}
		sort.Ints(idxs)
		for _, i := range idxs {
			b := builders[i]
			switch b.typ {
			case "text":
				msg.Blocks = append(msg.Blocks, Block{Type: "text", Text: b.text.String()})
			case "thinking":
				msg.Blocks = append(msg.Blocks, Block{Type: "thinking", Text: b.text.String(), Signature: b.sig.String()})
			case "redacted_thinking":
				// Opaque, but must be replayed verbatim in tool exchanges.
				msg.Blocks = append(msg.Blocks, Block{Type: "redacted_thinking", Data: b.data})
			case "tool_use":
				input := b.inputJSON.String()
				// A cancelled stream can leave half-received JSON here;
				// storing it would poison every later marshal.
				if strings.TrimSpace(input) == "" || !json.Valid([]byte(input)) {
					input = "{}"
				}
				msg.Blocks = append(msg.Blocks, Block{Type: "tool_use", ID: b.id, Name: b.name, Input: json.RawMessage(input)})
			}
		}
		if usage.InputTokens > 0 || usage.OutputTokens > 0 || usage.CacheReadTokens > 0 || usage.CacheWriteTokens > 0 {
			u := usage
			msg.Usage = &u
		}
		return msg
	}

	streamed := 0
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "" {
			continue
		}
		var ev sseEvent
		if err := json.Unmarshal([]byte(data), &ev); err != nil {
			continue
		}
		switch ev.Type {
		case "message_start":
			if ev.Message != nil {
				usage.InputTokens = ev.Message.Usage.InputTokens
				usage.OutputTokens = ev.Message.Usage.OutputTokens
				usage.CacheReadTokens = ev.Message.Usage.CacheReadTokens
				usage.CacheWriteTokens = ev.Message.Usage.CacheCreationTokens
			}
		case "content_block_start":
			if ev.ContentBlock != nil {
				builders[ev.Index] = &blockBuilder{typ: ev.ContentBlock.Type, id: ev.ContentBlock.ID, name: ev.ContentBlock.Name, data: ev.ContentBlock.Data}
				if ev.ContentBlock.Type == "tool_use" {
					out <- Event{Type: EventToolUseStart, ToolID: ev.ContentBlock.ID, ToolName: ev.ContentBlock.Name}
				}
			}
		case "content_block_delta":
			if len(builders) > maxStreamBlocks {
				return finish(), fmt.Errorf("response opened more than %d content blocks; stopping", maxStreamBlocks)
			}
			b := builders[ev.Index]
			if b == nil || ev.Delta == nil {
				continue
			}
			streamed += len(ev.Delta.Text) + len(ev.Delta.Thinking) +
				len(ev.Delta.PartialJSON) + len(ev.Delta.Signature)
			if overLimit(streamed) {
				return finish(), fmt.Errorf("response exceeded %d bytes; stopping", maxResponseBytes)
			}
			switch ev.Delta.Type {
			case "text_delta":
				b.text.WriteString(ev.Delta.Text)
				out <- Event{Type: EventTextDelta, Text: ev.Delta.Text}
			case "thinking_delta":
				b.text.WriteString(ev.Delta.Thinking)
				out <- Event{Type: EventThinkingDelta, Text: ev.Delta.Thinking}
			case "signature_delta":
				b.sig.WriteString(ev.Delta.Signature) // a Builder: += here was quadratic
			case "input_json_delta":
				b.inputJSON.WriteString(ev.Delta.PartialJSON)
				out <- Event{Type: EventToolInputDelta, Text: ev.Delta.PartialJSON, ToolID: b.id, ToolName: b.name}
			}
		case "content_block_stop":
			if b := builders[ev.Index]; b != nil && b.typ == "thinking" {
				out <- Event{Type: EventThinkingDone}
			}
		case "message_delta":
			if ev.Delta != nil && ev.Delta.StopReason != "" {
				stopReason = ev.Delta.StopReason
			}
			if ev.Usage != nil {
				usage.OutputTokens = ev.Usage.OutputTokens
			}
		case "error":
			m, typ := "stream error", ""
			if ev.Error != nil {
				m, typ = ev.Error.Message, ev.Error.Type
			}
			status := 400
			if typ == "overloaded_error" || typ == "api_error" {
				status = 529 // in-band server trouble is retryable
			}
			return finish(), &APIError{Provider: a.name, Status: status, Message: m}
		case "message_stop":
			out <- Event{Type: EventUsage, Usage: usage}
			out <- Event{Type: EventDone, StopReason: stopReason}
			return finish(), nil
		}
	}
	if ctx.Err() != nil {
		return finish(), ctx.Err()
	}
	if err := scanner.Err(); err != nil {
		return finish(), fmt.Errorf("anthropic: reading stream: %w", err)
	}
	if stopReason == "" {
		// Clean EOF without completion: a proxy cut the stream; retryable.
		return finish(), &APIError{Provider: a.name, Status: 502, Message: "stream ended before completion"}
	}
	out <- Event{Type: EventDone, StopReason: stopReason}
	return finish(), nil
}
