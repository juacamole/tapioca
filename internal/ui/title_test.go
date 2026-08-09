package ui

import "testing"

func TestCleanTitle(t *testing.T) {
	cases := []struct{ in, want string }{
		{"Fix the parser crash", "Fix the parser crash"},
		{"\"Quoted title\"", "Quoted title"},
		{"Title: Add retry logic", "Add retry logic"},
		{"**Bold title**", "Bold title"},
		{"Refactor the executor.", "Refactor the executor"},
		{"  spaced   out   words  ", "spaced out words"},
		{"<think>the user wants a title</think>\nFix the parser crash", "Fix the parser crash"},
		{"<thinking>hmm</thinking>Rename globMatch", "Rename globMatch"},
		{"Sure, here it is\nActual Title", "Sure, here it is"},
		{"", ""},
		{"<think>only reasoning, no answer</think>", ""},
	}
	for _, c := range cases {
		if got := cleanTitle(c.in); got != c.want {
			t.Errorf("cleanTitle(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestCleanTitleRejectsRambling(t *testing.T) {
	long := "I would be happy to help you with that request, and here is a title that " +
		"captures the essence of what you are asking me to do for this coding session"
	if got := cleanTitle(long); got != "" {
		t.Errorf("expected a rambling reply to be rejected, got %q", got)
	}
}

func TestCleanTitleTruncatesToWidth(t *testing.T) {
	in := "Implement the whole authentication subsystem including tokens"
	got := cleanTitle(in)
	if len([]rune(got)) > titleMaxLen {
		t.Errorf("title longer than %d: %q", titleMaxLen, got)
	}
	if got == "" {
		t.Error("a merely longish title should be truncated, not rejected")
	}
}
