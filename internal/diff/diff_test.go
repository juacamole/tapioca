package diff

import (
	"fmt"
	"strings"
	"testing"
)

// render turns ops into a compact string so expectations read like a diff.
func render(ops []Op) string {
	var b strings.Builder
	for _, op := range ops {
		if op.Kind == Skip {
			fmt.Fprintf(&b, "~%d\n", op.Count)
			continue
		}
		fmt.Fprintf(&b, "%c%s\n", op.Kind, op.Line)
	}
	return b.String()
}

func TestSingleLineChange(t *testing.T) {
	old := "a\nb\nc\n"
	new := "a\nB\nc\n"
	got := render(Lines(old, new))
	want := " a\n-b\n+B\n c\n"
	if got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
	added, removed := Stats(Lines(old, new))
	if added != 1 || removed != 1 {
		t.Errorf("stats = +%d -%d, want +1 -1", added, removed)
	}
}

func TestInsertAndDelete(t *testing.T) {
	if got, want := render(Lines("a\nc\n", "a\nb\nc\n")), " a\n+b\n c\n"; got != want {
		t.Errorf("insert got:\n%s\nwant:\n%s", got, want)
	}
	if got, want := render(Lines("a\nb\nc\n", "a\nc\n")), " a\n-b\n c\n"; got != want {
		t.Errorf("delete got:\n%s\nwant:\n%s", got, want)
	}
}

func TestIdenticalAndEmpty(t *testing.T) {
	if added, removed := Stats(Lines("same\n", "same\n")); added != 0 || removed != 0 {
		t.Errorf("identical text reported changes: +%d -%d", added, removed)
	}
	if ops := Lines("", ""); len(ops) != 0 {
		t.Errorf("empty/empty produced %d ops", len(ops))
	}
	if added, _ := Stats(Lines("", "new\n")); added != 1 {
		t.Error("creating content should count as an addition")
	}
	if _, removed := Stats(Lines("old\n", "")); removed != 1 {
		t.Error("clearing content should count as a removal")
	}
}

// A trailing newline is a formatting detail, not a changed line.
func TestTrailingNewlineIsNotAChange(t *testing.T) {
	if added, removed := Stats(Lines("a\nb", "a\nb\n")); added != 0 || removed != 0 {
		t.Errorf("trailing newline reported as a change: +%d -%d", added, removed)
	}
}

func TestLineNumbersTrackBothSides(t *testing.T) {
	ops := Lines("a\nb\nc\n", "a\nx\ny\nc\n")
	for _, op := range ops {
		switch {
		case op.Kind == Del && op.Line == "b":
			if op.OldNum != 2 || op.NewNum != 0 {
				t.Errorf("deleted line numbering wrong: %+v", op)
			}
		case op.Kind == Add && op.Line == "y":
			if op.NewNum != 3 || op.OldNum != 0 {
				t.Errorf("added line numbering wrong: %+v", op)
			}
		case op.Kind == Equal && op.Line == "c":
			if op.OldNum != 3 || op.NewNum != 4 {
				t.Errorf("trailing context numbering wrong: %+v", op)
			}
		}
	}
}

func TestCompactCollapsesUnchangedRuns(t *testing.T) {
	var oldLines, newLines []string
	for i := 0; i < 40; i++ {
		oldLines = append(oldLines, fmt.Sprintf("line %d", i))
		newLines = append(newLines, fmt.Sprintf("line %d", i))
	}
	newLines[20] = "CHANGED"

	ops := Compact(LinesOf(oldLines, newLines), 2)
	added, removed := Stats(ops)
	if added != 1 || removed != 1 {
		t.Fatalf("compaction lost the change: +%d -%d", added, removed)
	}
	equals := 0
	skips := 0
	for _, op := range ops {
		switch op.Kind {
		case Equal:
			equals++
		case Skip:
			skips++
		}
	}
	if equals != 4 {
		t.Errorf("kept %d context lines, want 4 (2 either side)", equals)
	}
	if skips != 2 {
		t.Errorf("got %d skip markers, want 2 (before and after)", skips)
	}
}

// Big rewrites fall back to delete-all/add-all rather than exhausting memory.
func TestHugeChangeFallsBack(t *testing.T) {
	var a, b []string
	for i := 0; i < maxDiffLines; i++ {
		a = append(a, fmt.Sprintf("old %d", i))
		b = append(b, fmt.Sprintf("new %d", i))
	}
	ops := LinesOf(a, b)
	added, removed := Stats(ops)
	if added != len(b) || removed != len(a) {
		t.Errorf("fallback lost lines: +%d -%d for %d/%d", added, removed, len(a), len(b))
	}
}
