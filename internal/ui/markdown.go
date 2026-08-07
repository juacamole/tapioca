package ui

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/charmbracelet/glamour"
)

// Models often emit sloppy emphasis like "** text **", which CommonMark
// (and therefore glamour) refuses to render. Normalize the common cases
// outside of code blocks before rendering.
var (
	sloppyBold   = regexp.MustCompile(`\*\*[ \t]*([^*\n]+?)[ \t]*\*\*`)
	sloppyBoldU  = regexp.MustCompile(`__[ \t]*([^_\n]+?)[ \t]*__`)
	sloppyHeader = regexp.MustCompile("^(#{1,6})([^#\\s])")
)

func normalizeMarkdown(text string) string {
	lines := strings.Split(text, "\n")
	inCode := false
	for i, l := range lines {
		trimmed := strings.TrimSpace(l)
		if strings.HasPrefix(trimmed, "```") || strings.HasPrefix(trimmed, "~~~") {
			inCode = !inCode
			continue
		}
		if inCode {
			continue
		}
		l = sloppyBold.ReplaceAllString(l, "**$1**")
		l = sloppyBoldU.ReplaceAllString(l, "__$1__")
		l = sloppyHeader.ReplaceAllString(l, "$1 $2") // "#Title" -> "# Title"
		lines[i] = l
	}
	return strings.Join(lines, "\n")
}

// Markdown rendering for finalized assistant messages via glamour, with a
// small cache: transcripts re-render on every event, but messages are
// immutable, so each (width, text) pair is rendered exactly once. Streaming
// text keeps the lightweight renderer and upgrades on completion.

var (
	mdRenderers = map[int]*glamour.TermRenderer{}
	mdCache     = map[string]string{}
	// mdStyle is decided once before the Bubble Tea program starts;
	// glamour's auto-detection queries the terminal, which is unsafe while
	// the TUI owns it.
	mdStyle = "dark"
)

const mdCacheMax = 300

// SetMarkdownDark picks the glamour style; call before the program runs.
func SetMarkdownDark(dark bool) {
	if dark {
		mdStyle = "dark"
	} else {
		mdStyle = "light"
	}
}

func mdRenderer(w int) *glamour.TermRenderer {
	if r, ok := mdRenderers[w]; ok {
		return r
	}
	r, err := glamour.NewTermRenderer(
		glamour.WithStandardStyle(mdStyle),
		glamour.WithWordWrap(w),
	)
	if err != nil {
		r = nil
	}
	mdRenderers[w] = r
	return r
}

// renderMarkdown renders markdown at the given width, falling back to the
// plain renderer on any failure.
func renderMarkdown(text string, w int) string {
	text = sanitizeText(text)
	if w < 10 {
		return renderText(text, w)
	}
	key := fmt.Sprintf("%d\x00%s", w, text)
	if out, ok := mdCache[key]; ok {
		return out
	}
	r := mdRenderer(w)
	if r == nil {
		return renderText(text, w)
	}
	out, err := r.Render(normalizeMarkdown(text))
	if err != nil {
		return renderText(text, w)
	}
	out = strings.Trim(out, "\n")
	if len(mdCache) >= mdCacheMax {
		mdCache = map[string]string{}
	}
	mdCache[key] = out
	return out
}
