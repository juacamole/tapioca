// Package diff computes line diffs for showing file edits in the chat.
package diff

import "strings"

// Kind marks what happened to a line.
type Kind byte

const (
	Equal Kind = ' '
	Add   Kind = '+'
	Del   Kind = '-'
	Skip  Kind = '~' // a run of unchanged lines that was collapsed
)

// Op is one line of a diff. OldNum/NewNum are 1-based, 0 where the line does
// not exist on that side. For Skip, Count holds how many lines were hidden.
type Op struct {
	Kind   Kind
	Line   string
	OldNum int
	NewNum int
	Count  int
}

// maxDiffLines caps the work: past this the algorithm's memory grows faster
// than the result is worth, and a whole-file replacement reads fine anyway.
const maxDiffLines = 4000

// Lines diffs two texts. Trailing newlines are ignored so a file ending in
// "\n" does not report a phantom empty line.
func Lines(oldText, newText string) []Op {
	a := splitLines(oldText)
	b := splitLines(newText)
	return LinesOf(a, b)
}

func splitLines(s string) []string {
	s = strings.TrimSuffix(s, "\n")
	if s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}

// LinesOf diffs pre-split lines.
func LinesOf(a, b []string) []Op {
	// Identical prefixes and suffixes are the common case for an edit; peeling
	// them off first keeps the expensive part proportional to the change.
	pre := 0
	for pre < len(a) && pre < len(b) && a[pre] == b[pre] {
		pre++
	}
	suf := 0
	for suf < len(a)-pre && suf < len(b)-pre && a[len(a)-1-suf] == b[len(b)-1-suf] {
		suf++
	}
	midA, midB := a[pre:len(a)-suf], b[pre:len(b)-suf]

	var ops []Op
	for i := 0; i < pre; i++ {
		ops = append(ops, Op{Kind: Equal, Line: a[i], OldNum: i + 1, NewNum: i + 1})
	}
	ops = append(ops, middle(midA, midB, pre)...)
	for i := 0; i < suf; i++ {
		oldIdx := len(a) - suf + i
		newIdx := len(b) - suf + i
		ops = append(ops, Op{Kind: Equal, Line: a[oldIdx], OldNum: oldIdx + 1, NewNum: newIdx + 1})
	}
	return ops
}

// middle diffs the differing span, falling back to a blunt delete-then-add
// when the span is too large to be worth a real diff.
func middle(a, b []string, offset int) []Op {
	if len(a) == 0 && len(b) == 0 {
		return nil
	}
	if len(a)+len(b) > maxDiffLines {
		var ops []Op
		for i, l := range a {
			ops = append(ops, Op{Kind: Del, Line: l, OldNum: offset + i + 1})
		}
		for i, l := range b {
			ops = append(ops, Op{Kind: Add, Line: l, NewNum: offset + i + 1})
		}
		return ops
	}
	return backtrack(a, b, trace(a, b), offset)
}

// maxDiffRounds bounds Myers' D, which is the number of differing lines. Past
// this the diff is a rewrite, and a rewrite reads better as a summary anyway.
const maxDiffRounds = 600

// trace runs Myers' algorithm, keeping each round's furthest-reaching paths so
// the edit script can be recovered afterwards.
func trace(a, b []string) [][]int {
	n, m := len(a), len(b)
	maxD := n + m
	// Each round snapshots the whole v array and every round is kept, so the
	// cost is O(D * (n+m)) memory. maxDiffLines bounds n+m but not D, and a
	// file rewritten line-for-line sits just under it: 2000 lines replaced by
	// 2000 others allocated ~300MB for a display-only diff. Bounding the work
	// makes the caller fall back to a plain summary instead.
	if maxD > maxDiffRounds {
		maxD = maxDiffRounds
	}
	v := make([]int, 2*(n+m)+1)
	var saved [][]int
	for d := 0; d <= maxD; d++ {
		snapshot := make([]int, len(v))
		copy(snapshot, v)
		saved = append(saved, snapshot)
		for k := -d; k <= d; k += 2 {
			var x int
			if k == -d || (k != d && v[maxD+k-1] < v[maxD+k+1]) {
				x = v[maxD+k+1]
			} else {
				x = v[maxD+k-1] + 1
			}
			y := x - k
			for x < n && y < m && a[x] == b[y] {
				x, y = x+1, y+1
			}
			v[maxD+k] = x
			if x >= n && y >= m {
				return saved
			}
		}
	}
	return saved
}

func backtrack(a, b []string, saved [][]int, offset int) []Op {
	n, m := len(a), len(b)
	maxD := n + m
	x, y := n, m
	var rev []Op
	for d := len(saved) - 1; d > 0 && (x > 0 || y > 0); d-- {
		v := saved[d]
		k := x - y
		var prevK int
		if k == -d || (k != d && v[maxD+k-1] < v[maxD+k+1]) {
			prevK = k + 1
		} else {
			prevK = k - 1
		}
		prevX := v[maxD+prevK]
		prevY := prevX - prevK
		for x > prevX && y > prevY {
			x, y = x-1, y-1
			rev = append(rev, Op{Kind: Equal, Line: a[x], OldNum: offset + x + 1, NewNum: offset + y + 1})
		}
		switch {
		case x > prevX:
			x--
			rev = append(rev, Op{Kind: Del, Line: a[x], OldNum: offset + x + 1})
		case y > prevY:
			y--
			rev = append(rev, Op{Kind: Add, Line: b[y], NewNum: offset + y + 1})
		}
	}
	for i, j := 0, len(rev)-1; i < j; i, j = i+1, j-1 {
		rev[i], rev[j] = rev[j], rev[i]
	}
	return rev
}

// Stats counts changed lines.
func Stats(ops []Op) (added, removed int) {
	for _, op := range ops {
		switch op.Kind {
		case Add:
			added++
		case Del:
			removed++
		}
	}
	return added, removed
}

// Compact keeps `context` unchanged lines around each change and replaces the
// rest with Skip markers, so a one-line edit in a long file stays readable.
func Compact(ops []Op, context int) []Op {
	keep := make([]bool, len(ops))
	for i, op := range ops {
		if op.Kind == Add || op.Kind == Del {
			for j := i - context; j <= i+context; j++ {
				if j >= 0 && j < len(ops) {
					keep[j] = true
				}
			}
		}
	}
	var out []Op
	run := 0
	for i, op := range ops {
		if keep[i] {
			if run > 0 {
				out = append(out, Op{Kind: Skip, Count: run})
				run = 0
			}
			out = append(out, op)
			continue
		}
		run++
	}
	if run > 0 {
		out = append(out, Op{Kind: Skip, Count: run})
	}
	return out
}
