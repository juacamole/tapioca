package ui

import (
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/reflow/wrap"

	"tapioca/internal/agent"
	"tapioca/internal/provider"
)

// dashActive mirrors whether any dashboard panel is on screen, so hints do not
// point at a dashboard that is not there. Set by refreshChat.
var dashActive bool

// Model output, tool results and file contents can carry raw terminal control
// sequences. Rendering them verbatim does more than corrupt the layout: OSC 52
// writes the user's clipboard, and a cursor-move plus clear can forge UI —
// including a fake permission prompt over the real one. Everything reaching
// the screen from outside this program goes through sanitizeText.
//
// C1 is stripped along with C0 because textenc's Latin-1 fallback turns bytes
// 0x80-0x9f into exactly those code points, and terminals in 8-bit mode act on
// them: U+009B is CSI, U+009D is OSC.
var (
	csiRe  = regexp.MustCompile("\x1b\\[[0-9;?]*[ -/]*[@-~]")
	oscRe  = regexp.MustCompile("\x1b\\][^\x07\x1b]*(\x07|\x1b\\\\)?")
	ctrlRe = regexp.MustCompile("[\x00-\x09\x0b-\x1f\x7f" + "\\x{0080}-\\x{009f}]")
)

func sanitizeText(s string) string {
	if utf8.ValidString(s) && strings.IndexFunc(s, unsafeRune) < 0 {
		return s
	}
	// Bytes that are not valid UTF-8 are dropped before anything else: a lone
	// 0x9b is CSI to a terminal in 8-bit mode, but decodes to U+FFFD here, so
	// neither the scan above nor the classes below would ever see it.
	s = strings.ToValidUTF8(s, "")
	s = strings.ReplaceAll(s, "\t", "    ")
	s = csiRe.ReplaceAllString(s, "")
	s = oscRe.ReplaceAllString(s, "")
	s = ctrlRe.ReplaceAllString(s, "")
	return s
}

func unsafeRune(r rune) bool {
	return (r < 0x20 && r != '\n') || r == 0x7f || (r >= 0x80 && r <= 0x9f)
}

// sanitizeLabel is sanitizeText for a one-line label: newlines would let a
// name draw extra rows and push the real content off screen.
func sanitizeLabel(s string) string {
	return strings.ReplaceAll(sanitizeText(s), "\n", " ")
}

// renderConversation renders an agent's full transcript at the given width,
// including any in-flight streaming output. In verbose mode thoughts, tool
// arguments and tool results are shown in full; otherwise they collapse to
// short summaries.
func renderConversation(a *agent.Agent, w int, spin string, verbose bool, open func(string) bool) string {
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
		parts = append(parts, renderMessageCached(&m, a, w, verbose, open))
	}

	if a.Status.Busy() || a.StreamText != "" || a.StreamThinking != "" {
		parts = append(parts, renderStreaming(a, w, spin, verbose))
	}
	for _, q := range a.Queue {
		label := q.Typed
		if label == "" {
			label = q.Text()
		}
		parts = append(parts, styDim.Render("queued | "+truncate(label, max(10, w-10))))
	}
	if a.LastErr != "" {
		parts = append(parts, styErr.Render("x "+wrapPlain(sanitizeText(a.LastErr), w-2)))
	}
	return strings.Join(parts, "\n\n")
}

func welcomeText(w int) string {
	// The mark is identity, not a hint, so zen mode keeps it and drops only
	// the keybind lines below.
	mark := renderWordmark(w)
	if zenMode {
		return strings.Join(mark, "\n")
	}
	lines := append(append([]string{}, mark...),
		"",
		styDim.Render("type a prompt and press enter to chat"),
		styDim.Render("f1 shows every keybind"+gl.sep+"/help lists commands"),
		styDim.Render("ctrl+g writes the prompt in vim"+gl.sep+"ctrl+o toggles verbose chat"),
	)
	if dashActive {
		lines = append(lines, styDim.Render("tab focuses the dashboard to change settings"))
	} else {
		lines = append(lines, styDim.Render("/panels adds dashboard panels"))
	}
	return strings.Join(lines, "\n")
}

// Messages are immutable once appended, so their rendered+wrapped form is
// cached; on long chats each streaming delta then costs only the tail.
var msgCache = map[string]string{}

const msgCacheMax = 2000

func renderMessageCached(m *provider.Message, a *agent.Agent, w int, verbose bool, open func(string) bool) string {
	size := 0
	for _, b := range m.Blocks {
		size += len(b.Text) + len(b.Content) + len(b.Input)
	}
	// Expanded thoughts change the rendering, so they belong in the cache key.
	thoughts := ""
	for bi, b := range m.Blocks {
		if b.Type == "thinking" {
			thoughts += fmt.Sprintf("%d%t", bi, open(thinkKey(m, bi)))
		}
	}
	key := fmt.Sprintf("%d|%d|%t|%s|%d|%d|%d|%s", a.ID, w, verbose, m.Role, m.Time.UnixNano(), len(m.Blocks), size, thoughts)
	if s, ok := msgCache[key]; ok {
		return s
	}
	s := wrap.String(renderMessage(m, a, w, verbose, open), w)
	if len(msgCache) >= msgCacheMax {
		msgCache = map[string]string{}
	}
	msgCache[key] = s
	return s
}

func renderMessage(m *provider.Message, a *agent.Agent, w int, verbose bool, open func(string) bool) string {
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
		b.WriteString(styUser.Render(gl.bar+" you ") + ts + "\n")
		b.WriteString(renderText(m.Text(), w))
		for _, bl := range m.Blocks {
			if bl.Type == "image" {
				b.WriteString("\n" + styTool.Render(fmt.Sprintf("[image %s, %dkB]", bl.MediaType, len(bl.Data)*3/4/1024)))
			}
		}
		return b.String()
	}

	// Assistant message.
	col := lipgloss.NewStyle().Bold(true).Foreground(agentColor(a.ID))
	head := col.Render(gl.bar+" "+a.Name) + styDim.Render(""+gl.sep+""+m.Model+""+gl.sep+"") + ts
	b.WriteString(head)
	for i, bl := range m.Blocks {
		switch bl.Type {
		case "thinking":
			b.WriteString("\n" + renderThinking(bl.Text, w, open(thinkKey(m, i))))
		case "redacted_thinking":
			b.WriteString("\n" + styThink.Render("thought (redacted by the provider)"))
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

// renderThinking renders a finished thinking block, collapsed to its header
// unless expanded.
func renderThinking(text string, w int, open bool) string {
	text = sanitizeText(text)
	if !open {
		return styThink.Render(thinkHeader(text, false))
	}
	var b strings.Builder
	b.WriteString(styThink.Render(thinkHeader(text, true)))
	for _, l := range strings.Split(wrapPlain(text, w-2), "\n") {
		b.WriteString("\n" + styThink.Render("  "+l))
	}
	return b.String()
}

// renderToolUse renders a tool invocation by the model.
func renderToolUse(bl *provider.Block, w int, verbose bool) string {
	name := sanitizeLabel(bl.Name)
	head := styTool.Render("tool: " + name)
	args := sanitizeText(strings.TrimSpace(string(bl.Input)))
	if args == "" || args == "{}" {
		return head
	}
	if !verbose {
		return head + " " + styDim.Render(truncate(args, max(10, w-lipgloss.Width(name)-8)))
	}
	var b strings.Builder
	b.WriteString(head)
	for _, l := range strings.Split(wrapPlain(args, w-2), "\n") {
		b.WriteString("\n" + styDim.Render("  "+l))
	}
	return b.String()
}

// diffLine colors a formatted diff row by its marker: the eye should find the
// change without reading line numbers.
func diffLine(l string, w int) string {
	trimmed := truncate(l, w)
	marker := ""
	if f := strings.Fields(l); len(f) > 1 {
		marker = f[1]
	}
	switch marker {
	case "+":
		return styOK.Render(trimmed)
	case "-":
		return styErr.Render(trimmed)
	default:
		return styDim.Render(trimmed)
	}
}

// renderChange shows a file edit as a header plus colored +/- lines. The rows
// carry file contents, so a checked-in escape sequence would otherwise reach
// the terminal through a diff of a file nobody chose to display.
func renderChange(display string, w int) string {
	lines := strings.Split(sanitizeText(display), "\n")
	out := []string{styTool.Render(truncate(lines[0], w))}
	for _, l := range lines[1:] {
		out = append(out, "  "+diffLine(l, w-2))
	}
	return strings.Join(out, "\n")
}

func renderToolResult(bl *provider.Block, w int, verbose bool) string {
	if bl.Display != "" {
		// The diff says everything the terse "edited x (1 replacement)" did.
		return renderChange(bl.Display, w)
	}
	icon := styOK.Render("ok")
	if bl.IsError {
		icon = styErr.Render("err")
	}
	head := styTool.Render(sanitizeLabel(bl.Name)) + styDim.Render(" -> ") + icon
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
	b.WriteString(col.Render(gl.bar+" "+a.Name) + styDim.Render(""+gl.sep+""+statusLabel(a)+" "+spin))

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
		b.WriteString("\n" + renderText(a.StreamText+gl.bar, w))
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
	// The ellipsis is one rune in unicode but three in ascii; both have to fit
	// inside n or every truncated label overflows its column.
	e := []rune(gl.ellipsis)
	if len(e) >= n {
		return string(r[:n])
	}
	return string(r[:n-len(e)]) + gl.ellipsis
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
