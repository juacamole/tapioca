package ui

import (
	"regexp"
	"strings"
)

// URLs in the transcript were dead text: a status page in a provider error, a
// doc the model quoted, a pull request it had just opened — all of them had to
// be selected, copied and pasted by hand. Terminals have carried clickable
// hyperlinks for years and Tapioca emitted none.
//
// OSC 8 is the mechanism: the URL travels beside the text rather than as it,
// so a terminal that understands the sequence makes the text clickable and one
// that does not shows the text unchanged.

// urlRe matches a bare http(s) URL. Deliberately conservative: it stops at
// whitespace and at the punctuation that usually ends a sentence rather than
// belongs to the address, so "see https://example.com." does not produce a
// link with a full stop in it.
//
// It also stops at any control byte. This runs on rendered text, where a URL
// is immediately followed by the escape that ends its styling — without that
// exclusion the match swallowed the escape and the whole thing was rejected as
// unlinkable, which is how the markdown path silently produced no links.
var urlRe = regexp.MustCompile(`https?://[^\s<>"'` + "`" + `\x00-\x1f\x7f]*[^\s<>"'` + "`" + `\x00-\x1f\x7f.,;:!?)\]}]`)

// linkable reports whether a matched string is safe to put in an OSC 8 target.
// Everything reaching here has already been through sanitizeText, so escapes
// are gone; this is the second check rather than the first, because the cost
// of being wrong is a sequence the terminal acts on.
func linkable(u string) bool {
	if len(u) > 2048 {
		return false // a target that long is not a link anyone meant to click
	}
	for _, r := range u {
		// C0, DEL and C1: none can appear in a URL, and each is a way out of
		// the sequence.
		if r < 0x20 || r == 0x7f || (r >= 0x80 && r <= 0x9f) {
			return false
		}
	}
	return true
}

// linkify wraps bare URLs in OSC 8 hyperlinks.
//
// It runs on already-rendered text. That is safe because the renderer keeps a
// URL contiguous — glamour styles the whole address as one span — so a match
// never straddles a colour code.
//
// The ascii glyph set is left alone. It exists for terminals that cannot be
// assumed to handle much, and a stray escape there is worse than a link that
// has to be copied.
func linkify(s string) string {
	if gl.plainText || s == "" {
		return s
	}
	return urlRe.ReplaceAllStringFunc(s, func(u string) string {
		if !linkable(u) {
			return u
		}
		// OSC 8 ; params ; target ST — text — OSC 8 ; ; ST. The terminator is
		// ST (ESC \) rather than BEL: both are accepted, and ST is what the
		// specification uses.
		return "\x1b]8;;" + u + "\x1b\\" + u + "\x1b]8;;\x1b\\"
	})
}

// hasHyperlink reports whether a rendered string carries an OSC 8 link. Used
// by tests, and by anything that needs to tell our own sequences from text.
func hasHyperlink(s string) bool { return strings.Contains(s, "\x1b]8;;") }
