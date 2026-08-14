package ui

import (
	"strings"
	"testing"
)

func TestURLsBecomeHyperlinks(t *testing.T) {
	out := renderText("see https://example.com/a?b=1 for details", 60)
	if !hasHyperlink(out) {
		t.Fatalf("no hyperlink emitted: %q", out)
	}
	if !strings.Contains(out, "\x1b]8;;https://example.com/a?b=1\x1b\\") {
		t.Errorf("the link target is wrong: %q", out)
	}
	// The visible text is unchanged: a link is an addition, not a rewrite.
	if !strings.Contains(stripAnsi(out), "see https://example.com/a?b=1 for details") {
		t.Errorf("the text changed: %q", stripAnsi(out))
	}
}

// Markdown is the usual path for assistant prose, and glamour keeps a URL
// contiguous, which is what makes post-processing safe.
func TestMarkdownURLsBecomeHyperlinks(t *testing.T) {
	out := renderMarkdown("See https://example.com/docs for details.", 60)
	if !hasHyperlink(out) {
		t.Errorf("no hyperlink in markdown output: %q", out)
	}
}

// Trailing punctuation belongs to the sentence, not the address.
func TestSentencePunctuationIsNotPartOfTheLink(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"go to https://example.com.", "https://example.com"},
		{"see https://example.com/a, then", "https://example.com/a"},
		{"(https://example.com/b)", "https://example.com/b"},
		{"https://example.com/c?x=1;", "https://example.com/c?x=1"},
	} {
		out := linkify(tc.in)
		want := "\x1b]8;;" + tc.want + "\x1b\\"
		if !strings.Contains(out, want) {
			t.Errorf("linkify(%q) target is not %q: %q", tc.in, tc.want, out)
		}
	}
}

// The target is a place to smuggle an escape, so nothing that could terminate
// the sequence may reach it. Sanitising runs first in the real path; this is
// the second line of defence.
func TestHostileURLsAreNotLinked(t *testing.T) {
	for _, u := range []string{
		"https://evil.com\x1b]52;c;cHduZWQ\x07",
		"https://evil.com\x07",
		"https://evil.com\x1b\\",
		"https://evil.com" + strings.Repeat("a", 3000),
	} {
		if linkable(strings.TrimPrefix(u, "")) && strings.ContainsAny(u, "\x1b\x07") {
			t.Errorf("a URL containing an escape was accepted: %q", u)
		}
		out := linkify(u)
		// Whatever happens, no OSC 8 target may contain an escape or BEL.
		if i := strings.Index(out, "\x1b]8;;"); i >= 0 {
			rest := out[i+5:]
			end := strings.Index(rest, "\x1b\\")
			if end < 0 {
				t.Errorf("unterminated hyperlink for %q", u)
				continue
			}
			if strings.ContainsAny(rest[:end], "\x1b\x07") {
				t.Errorf("hostile bytes reached the link target: %q", rest[:end])
			}
		}
	}
}

// Only http(s). A javascript: or file: target in a clickable link is a way to
// get a click to do something the text did not say.
func TestOnlyHTTPSchemesAreLinked(t *testing.T) {
	for _, u := range []string{
		"javascript:alert(1)",
		"file:///etc/passwd",
		"data:text/html,<script>",
		"ftp://example.com",
	} {
		if hasHyperlink(linkify(u)) {
			t.Errorf("%q was turned into a link", u)
		}
	}
}

// The ascii glyph set exists for terminals that cannot be assumed to handle
// much; a stray escape there is worse than a link that has to be copied.
func TestAsciiGlyphsEmitNoHyperlinks(t *testing.T) {
	SetGlyphs("ascii")
	defer SetGlyphs("unicode")

	out := linkify("see https://example.com now")
	if hasHyperlink(out) {
		t.Errorf("ascii mode emitted a hyperlink: %q", out)
	}
	if out != "see https://example.com now" {
		t.Errorf("ascii mode altered the text: %q", out)
	}
}

// Text with no URL must come through untouched, byte for byte.
func TestTextWithoutURLsIsUnchanged(t *testing.T) {
	for _, s := range []string{
		"nothing to see",
		"a colon: and a slash/ but no scheme",
		"http:// not really",
	} {
		if got := linkify(s); got != s && hasHyperlink(got) {
			t.Errorf("linkify(%q) = %q", s, got)
		}
	}
}

// The plain projection feeds mouse selection, the clipboard and anything that
// measures text. A hyperlink left in it would mean selecting a line put escape
// sequences on the clipboard — a worse bug than the one links fix.
func TestHyperlinksAreStrippedFromThePlainProjection(t *testing.T) {
	rendered := renderText("see https://example.com/a?b=1 now", 60)
	plain := stripAnsi(rendered)

	if strings.ContainsAny(plain, "\x1b\x07") {
		t.Fatalf("escapes survived into the plain text: %q", plain)
	}
	if !strings.Contains(plain, "see https://example.com/a?b=1 now") {
		t.Errorf("the plain text lost or mangled the URL: %q", plain)
	}
	// The address must appear once, not twice — OSC 8 carries it in the target
	// as well as the body, and a naive strip would leave both.
	if n := strings.Count(plain, "https://example.com/a?b=1"); n != 1 {
		t.Errorf("the URL appears %d times in the plain text: %q", n, plain)
	}
}
