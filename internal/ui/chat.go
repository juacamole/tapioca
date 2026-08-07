package ui

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/reflow/wrap"

	"tapioca/internal/agent"
	"tapioca/internal/provider"
)

// Model output and tool results can carry raw terminal control sequences
// (colors, \r progress bars, cursor movement) that would corrupt the TUI if
// rendered verbatim.
var (
	csiRe  = regexp.MustCompile("\x1b\\[[0-9;?]*[ -/]*[@-~]")
	oscRe  = regexp.MustCompile("\x1b\\][^\x07\x1b]*(\x07|\x1b\\\\)?")
	ctrlRe = regexp.MustCompile("[\x00-\x09\x0b-\x1f\x7f]")
)

func sanitizeText(s string) string {
	if strings.IndexFunc(s, func(r rune) bool { return (r < 0x20 && r != '\n') || r == 0x7f }) < 0 {
		return s
	}
	s = strings.ReplaceAll(s, "\t", "    ")
	s = csiRe.ReplaceAllString(s, "")
	s = oscRe.ReplaceAllString(s, "")
	s = ctrlRe.ReplaceAllString(s, "")
	return s
}

// renderConversation renders an agent's full transcript at the given width,
// including any in-flight streaming output. In verbose mode thoughts, tool
// arguments and tool results are shown in full; otherwise they collapse to
// short summaries.
func renderConversation(a *agent.Agent, w int, spin string, verbose bool) string {
	if w < 10 {
		w = 10
	}
	var parts []string

	if len(a.Messages) == 0 && a.StreamText == "" && a.StreamThinking == "" {
		parts = append(parts, welcomeText(w))
	}

	for _, m := range a.Messages {
		if m.Hidden {
			continue
		}
		parts = append(parts, renderMessageCached(&m, a, w, verbose))
	}

	if a.Status.Busy() || a.StreamText != "" || a.StreamThinking != "" {
		parts = append(parts, renderStreaming(a, w, spin, verbose))
	}
	for _, q := range a.Queue {
		parts = append(parts, styDim.Render("queued | "+truncate(q, max(10, w-10))))
	}
	if a.LastErr != "" {
		parts = append(parts, styErr.Render("x "+wrapPlain(a.LastErr, w-2)))
	}
	return strings.Join(parts, "\n\n")
}

func welcomeText(w int) string {
	if zenMode {
		return styAppTitle.Render("tapioca")
	}
	lines := []string{
		styAppTitle.Render("tapioca"),
		"",
		styDim.Render("type a prompt and press enter to chat"),
		styDim.Render("f1 shows every keybind · /help lists commands"),
		styDim.Render("ctrl+g writes the prompt in vim · ctrl+o toggles verbose chat"),
		styDim.Render("tab focuses the dashboard to change settings"),
	}
	return strings.Join(lines, "\n")
}

// Messages are immutable once appended, so their rendered+wrapped form is
// cached; on long chats each streaming delta then costs only the tail.
var msgCache = map[string]string{}

const msgCacheMax = 2000

func renderMessageCached(m *provider.Message, a *agent.Agent, w int, verbose bool) string {
	size := 0
	for _, b := range m.Blocks {
		size += len(b.Text) + len(b.Content) + len(b.Input)
	}
	key := fmt.Sprintf("%d|%d|%t|%s|%d|%d|%d", a.ID, w, verbose, m.Role, m.Time.UnixNano(), len(m.Blocks), size)
	if s, ok := msgCache[key]; ok {
		return s
	}
	s := wrap.String(renderMessage(m, a, w, verbose), w)
	if len(msgCache) >= msgCacheMax {
		msgCache = map[string]string{}
	}
	msgCache[key] = s
	return s
}

func renderMessage(m *provider.Message, a *agent.Agent, w int, verbose bool) string {
	ts := styDim.Render(m.Time.Format("15:04"))
	var b strings.Builder

	// Local notes (slash commands); shown in the transcript, never sent to
	// the model.
	if m.Role == "note" {
		return styDim.Render("> ") + styAccent.Render(truncate(m.Text(), max(10, w-8))) + " " + ts
	}

	if m.Role == "user" {
		if m.IsToolResult() {
			for i, bl := range m.Blocks {
				if i > 0 {
					b.WriteString("\n")
				}
				b.WriteString(renderToolResult(&bl, w, verbose))
			}
			return b.String()
		}
		b.WriteString(styUser.Render("| you ") + ts + "\n")
		b.WriteString(renderText(m.Text(), w))
		return b.String()
	}

	// Assistant message.
	col := lipgloss.NewStyle().Bold(true).Foreground(agentColor(a.ID))
	head := col.Render("| "+a.Name) + styDim.Render(" · "+m.Model+" · ") + ts
	b.WriteString(head)
	for _, bl := range m.Blocks {
		switch bl.Type {
		case "thinking":
			b.WriteString("\n" + renderThinking(bl.Text, w, verbose))
		case "text":
			if strings.TrimSpace(bl.Text) != "" {
				b.WriteString("\n" + renderMarkdown(bl.Text, w))
			}
		case "tool_use":
			b.WriteString("\n" + renderToolUse(&bl, w, verbose))
		}
	}
	if m.Usage != nil {
		b.WriteString("\n" + styDim.Render(fmt.Sprintf("  %d->%d tok", m.Usage.InputTokens, m.Usage.OutputTokens)))
	}
	return b.String()
}

// renderThinking renders a finished thinking block.
func renderThinking(text string, w int, verbose bool) string {
	text = sanitizeText(text)
	if !verbose {
		return styThink.Render(fmt.Sprintf("thought (%s)", humanTokens(len(text))))
	}
	var b strings.Builder
	b.WriteString(styThink.Render(fmt.Sprintf("thinking (%s)", humanTokens(len(text)))))
	for _, l := range strings.Split(wrapPlain(text, w-2), "\n") {
		b.WriteString("\n" + styThink.Render("  "+l))
	}
	return b.String()
}

// renderToolUse renders a tool invocation by the model.
func renderToolUse(bl *provider.Block, w int, verbose bool) string {
	head := styTool.Render("tool: " + bl.Name)
	args := sanitizeText(strings.TrimSpace(string(bl.Input)))
	if args == "" || args == "{}" {
		return head
	}
	if !verbose {
		return head + " " + styDim.Render(truncate(args, max(10, w-lipgloss.Width(bl.Name)-8)))
	}
	var b strings.Builder
	b.WriteString(head)
	for _, l := range strings.Split(wrapPlain(args, w-2), "\n") {
		b.WriteString("\n" + styDim.Render("  "+l))
	}
	return b.String()
}

func renderToolResult(bl *provider.Block, w int, verbose bool) string {
	icon := styOK.Render("ok")
	if bl.IsError {
		icon = styErr.Render("err")
	}
	head := styTool.Render(bl.Name) + styDim.Render(" -> ") + icon
	body := sanitizeText(strings.TrimRight(bl.Content, "\n"))
	if strings.TrimSpace(body) == "" {
		return head
	}
	if verbose {
		var lines []string
		for _, l := range strings.Split(wrapPlain(body, w-2), "\n") {
			lines = append(lines, styDim.Render("  "+l))
		}
		return head + "\n" + strings.Join(lines, "\n")
	}
	lines := strings.Split(body, "\n")
	const maxLines = 3
	trimmed := len(lines) > maxLines
	if trimmed {
		lines = lines[:maxLines]
	}
	for i, l := range lines {
		lines[i] = styDim.Render("  " + truncate(l, w-4))
	}
	if trimmed {
		lines = append(lines, styDim.Render("  … (ctrl+o for full output)"))
	}
	return head + "\n" + strings.Join(lines, "\n")
}

// renderStreaming renders the in-flight assistant message.
func renderStreaming(a *agent.Agent, w int, spin string, verbose bool) string {
	col := lipgloss.NewStyle().Bold(true).Foreground(agentColor(a.ID))
	var b strings.Builder
	b.WriteString(col.Render("| "+a.Name) + styDim.Render(" · "+statusLabel(a)+" "+spin))

	if a.StreamThinking != "" {
		if verbose {
			b.WriteString("\n" + styThink.Render(fmt.Sprintf("thinking (%s)", humanTokens(len(a.StreamThinking)))))
			for _, l := range strings.Split(wrapPlain(sanitizeText(a.StreamThinking), w-2), "\n") {
				b.WriteString("\n" + styThink.Render("  "+l))
			}
		} else if a.Status == agent.StatusThinking {
			wrapped := wrapPlain(sanitizeText(a.StreamThinking), w-2)
			lines := strings.Split(wrapped, "\n")
			if len(lines) > 4 {
				lines = lines[len(lines)-4:]
			}
			b.WriteString("\n" + styThink.Render("thinking…"))
			for _, l := range lines {
				b.WriteString("\n" + styThink.Render("  "+l))
			}
		} else {
			b.WriteString("\n" + styThink.Render(fmt.Sprintf("thought (%s)", humanTokens(len(a.StreamThinking)))))
		}
	}
	if a.StreamText != "" {
		b.WriteString("\n" + renderText(a.StreamText+"▌", w))
	}
	return b.String()
}

// renderText renders message text with light markdown styling: fenced code
// blocks get a tinted background, everything else is wrapped.
func renderText(text string, w int) string {
	text = sanitizeText(text)
	text = strings.ReplaceAll(text, "\t", "    ")
	lines := strings.Split(text, "\n")
	var out []string
	var chunk []string
	inCode := false

	flush := func() {
		if len(chunk) == 0 {
			return
		}
		joined := strings.Join(chunk, "\n")
		if inCode {
			for _, l := range strings.Split(joined, "\n") {
				out = append(out, styCode.Render(padRight(truncate(l, w), w)))
			}
		} else {
			out = append(out, lipgloss.NewStyle().Width(w).Render(joined))
		}
		chunk = nil
	}

	for _, l := range lines {
		if strings.HasPrefix(strings.TrimSpace(l), "```") {
			flush()
			inCode = !inCode
			out = append(out, styDim.Render(truncate(l, w)))
			continue
		}
		chunk = append(chunk, l)
	}
	// A dangling open fence still renders as code.
	flush()
	return strings.Join(out, "\n")
}

// wrapPlain wraps text without styling.
func wrapPlain(s string, w int) string {
	if w < 4 {
		w = 4
	}
	return lipgloss.NewStyle().Width(w).Render(s)
}

func truncate(s string, n int) string {
	if n < 1 {
		n = 1
	}
	s = strings.ReplaceAll(s, "\n", " ")
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}

func padRight(s string, n int) string {
	if d := n - lipgloss.Width(s); d > 0 {
		return s + strings.Repeat(" ", d)
	}
	return s
}

// humanTokens reports an estimated token count for a text of n characters
// (tokens are the app-wide unit of measure; ~4 chars per token).
func humanTokens(n int) string {
	t := n / 4
	if n > 0 && t == 0 {
		t = 1
	}
	return fmt.Sprintf("%s tok", humanInt(t))
}

func humanInt(n int) string {
	s := fmt.Sprintf("%d", n)
	if len(s) <= 3 {
		return s
	}
	var b strings.Builder
	pre := len(s) % 3
	if pre > 0 {
		b.WriteString(s[:pre])
	}
	for i := pre; i < len(s); i += 3 {
		if b.Len() > 0 {
			b.WriteString(",")
		}
		b.WriteString(s[i : i+3])
	}
	return b.String()
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
