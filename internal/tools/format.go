package tools

import (
	"fmt"
	"strings"

	"tapioca/internal/diff"
)

const (
	diffContext  = 2  // unchanged lines kept around each change
	diffMaxLines = 40 // beyond this the transcript turns into a file dump
)

// FormatChange renders an edit as plain text for the transcript: a header
// naming the file and the counts, then the changed lines. The UI colors it by
// line prefix, so no styling is baked in here.
func FormatChange(c *FileChange) string {
	if c == nil {
		return ""
	}
	var b strings.Builder
	verb := "updated"
	if c.Created {
		verb = "created"
	}
	fmt.Fprintf(&b, "%s %s (+%d -%d)\n", verb, c.Path, c.Added, c.Removed)

	shown := 0
	for _, op := range diff.Compact(c.Ops, diffContext) {
		if shown >= diffMaxLines {
			fmt.Fprintf(&b, "… %d more lines\n", remaining(c, diffMaxLines))
			break
		}
		switch op.Kind {
		case diff.Skip:
			fmt.Fprintf(&b, "  ⋮ %d unchanged\n", op.Count)
		case diff.Add:
			fmt.Fprintf(&b, "%5d + %s\n", op.NewNum, op.Line)
			shown++
		case diff.Del:
			fmt.Fprintf(&b, "%5d - %s\n", op.OldNum, op.Line)
			shown++
		default:
			fmt.Fprintf(&b, "%5d   %s\n", op.NewNum, op.Line)
			shown++
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

// remaining reports how many changed lines the display left out. It counts from
// the header's totals rather than from c.Ops, because the op list is capped at
// maxKeptOps and recounting it would say "12 more lines" for a rewrite of a
// hundred thousand.
func remaining(c *FileChange, shown int) int {
	n := c.Added + c.Removed
	if n <= shown {
		return 0
	}
	return n - shown
}
