package cli

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"tapioca/internal/session"
)

// --list-sessions prints the directory each session was recorded in. That
// directory is the project's own path, and in this threat model the project is
// an extracted archive that chose its own names — tar will create a directory
// called anything at all, including an escape sequence, and one run of tapioca
// inside it puts the name in the index for every later listing.
//
// The TUI's picker sanitizes every item it renders. This listing wrote to
// stdout with nothing in the way, so the same bytes reached a terminal here and
// not there.
func TestTheSessionListingDoesNotWriteEscapesToTheTerminal(t *testing.T) {
	for _, tc := range []struct {
		name string
		meta session.Meta
	}{
		{"an escape in the recorded directory", session.Meta{
			ID: "20260101-000000-aaaaaa", Name: "ordinary",
			Cwd: "/home/you/\x1b]52;c;cGF5bG9hZA==\x07evil"}},
		{"a clear-screen in the recorded directory", session.Meta{
			ID: "20260101-000000-bbbbbb", Name: "ordinary",
			Cwd: "/home/you/\x1b[2J\x1b[Hevil"}},
		{"a newline forging a second row", session.Meta{
			ID: "20260101-000000-cccccc", Name: "ordinary",
			Cwd: "/home/you/a\n20260101-000000-dddddd  something-that-is-not-a-session"}},
		{"an escape in the stored name", session.Meta{
			ID: "20260101-000000-eeeeee", Name: "\x1b[31mred\x1b[0m", Cwd: "/home/you/p"}},
		{"a C1 introducer, which is one byte and not an ESC", session.Meta{
			ID: "20260101-000000-ffffff", Name: "ordinary", Cwd: "/home/you/\u009b2Jevil"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// The control: the formatting this listing used before, which is a
			// plain Printf of the same fields. If the case does not reach a
			// terminal unfiltered that way, it shows nothing.
			unfiltered := fmt.Sprintf("%s  %-32s  %d agents  %d msgs  %s  %s",
				tc.meta.ID, tc.meta.Name, tc.meta.Agents, tc.meta.Messages,
				tc.meta.UpdatedAt.Format("2006-01-02 15:04"), tc.meta.Cwd)
			if !strings.ContainsFunc(unfiltered, func(r rune) bool {
				return (r < 0x20 && r != ' ') || r == 0x7f || (r >= 0x80 && r <= 0x9f)
			}) {
				t.Skip("control: this case carries nothing a terminal would act on")
			}

			got := sessionLine(tc.meta)
			for _, r := range got {
				if (r < 0x20 && r != ' ') || r == 0x7f || (r >= 0x80 && r <= 0x9f) {
					t.Fatalf("a control character reached stdout: %q in %q", r, got)
				}
			}
			if strings.Contains(got, "\n") {
				t.Fatalf("the row was split in two: %q", got)
			}
			// The row still has to be the row: an unreadable listing is not a
			// fix, it is a different bug.
			if !strings.Contains(got, tc.meta.ID) {
				t.Errorf("the session id is missing: %q", got)
			}
		})
	}
}

// The ordinary half: an everyday session still reads the way it did.
func TestAnOrdinarySessionListingIsUnchanged(t *testing.T) {
	when := time.Date(2026, 2, 3, 4, 5, 0, 0, time.UTC)
	got := sessionLine(session.Meta{
		ID: "20260203-040500-abc123", Name: "fix the parser",
		Cwd: "/home/you/projects/tapioca", Agents: 2, Messages: 41, UpdatedAt: when,
	})
	for _, want := range []string{
		"20260203-040500-abc123", "fix the parser", "2 agents", "41 msgs",
		"2026-02-03 04:05", "/home/you/projects/tapioca",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("the row lost %q: %q", want, got)
		}
	}
}

// An unnamed session and one saved before directories were recorded still say
// so rather than showing a blank column — and a name that is nothing but
// control characters counts as unnamed, since stripping it leaves nothing.
func TestTheSessionListingStillLabelsWhatIsMissing(t *testing.T) {
	got := sessionLine(session.Meta{ID: "id", Name: "\x1b\x07\x00", Cwd: ""})
	if !strings.Contains(got, "(unnamed)") {
		t.Errorf("a name that sanitized away is not labelled: %q", got)
	}
	if !strings.Contains(got, "(no project recorded)") {
		t.Errorf("a missing directory is not labelled: %q", got)
	}
}
