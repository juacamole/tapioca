package project

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// importFanout lays out a tree whose AGENTS.md imports the same megabyte over
// and over under a different spelling each time. `d` is a symlink to the
// directory it sits in, which git stores and a tarball carries, so `d/big.md`,
// `d/d/big.md`, `d/d/d/big.md` … are one file reached by n paths.
//
// Nothing lexical collapses them: filepath.Abs cleans, and Clean cannot remove
// a component that is a symlink. So the `seen` map — keyed on the path, not on
// the file — never fires, and every root check passes, because every one of
// those paths really does resolve inside the project and really does end .md.
func importFanout(t *testing.T, imports, kb int) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("TAPIOCA_CONFIG_DIR", t.TempDir())
	if err := os.MkdirAll(filepath.Join(dir, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(".", filepath.Join(dir, "d")); err != nil {
		t.Skipf("cannot create a symlink here: %v", err)
	}
	line := "lorem ipsum dolor sit amet consectetur\n" // 39 bytes
	body := strings.Repeat(line, kb*1024/len(line))
	if err := os.WriteFile(filepath.Join(dir, "big.md"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	prefix := "d"
	for i := 0; i < imports; i++ {
		fmt.Fprintf(&b, "@%s/big.md\n", prefix)
		prefix += "/d"
	}
	if err := os.WriteFile(filepath.Join(dir, "AGENTS.md"), []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

// maxInstructionFile bounds one file and maxInstructions bounds the joined
// result, and neither bounds what the expansion costs on the way between them:
// the imported bodies are all held in `lines` until the join, so the peak is
// the sum of every import, not the 80 KB that survives.
//
// Instructions() runs while the system prompt is built and again on /cd, with
// no deadline and nothing to cancel — the same shape as the FIFO that wedged
// the first turn. A two-megabyte checkout is enough to make the peak
// hundreds of megabytes, and scaling the fixture up is a matter of adding
// lines to a file that is itself capped at a megabyte.
func TestImportsAreBoundedInAggregate(t *testing.T) {
	const (
		imports = 600
		kb      = 200
	)
	dir := importFanout(t, imports, kb)

	// The control: one import of that size really is expanded, so the fixture
	// is doing what it claims and a small figure below means the expansion
	// stopped rather than that nothing was ever read.
	solo := importFanout(t, 1, kb)
	if n := len(Instructions(solo)); n < maxInstructions {
		t.Skipf("a single import of %d KB expanded to only %d bytes; the fixture is not exercising imports", kb, n)
	}

	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	out := Instructions(dir)
	runtime.ReadMemStats(&after)
	grew := after.TotalAlloc - before.TotalAlloc

	// What is actually being asserted is that the expansion stops at the
	// aggregate budget, and that is visible in the output: the marker is
	// written exactly where reading stopped. Allocation is the symptom and is
	// reported rather than asserted on — it moves with the allocator and the
	// GC, and a threshold tight enough to catch the unbounded case (281 MB
	// here) sits close enough to the bounded one to fail on a different
	// machine, which is what it did on CI at 33 MB against a 32 MB budget.
	t.Logf("expanding %d imports of %d KB allocated %d MB", imports, kb, grew>>20)
	if len(out) > maxInstructions+len("\n[truncated]") {
		t.Errorf("the joined result is %d bytes, past its own cap", len(out))
	}

	// The number is deliberately loose. Measured: 281 MB unbounded against
	// 24 MB bounded here and 33 MB on CI, where the allocator and the GC land
	// differently. A threshold at the bounded figure fails on whichever machine
	// runs it slightly differently — it already did, at 33 against 32 — so this
	// sits three times clear of the bounded case and three times under the
	// unbounded one. A ratio would be prettier and does not work: ten times the
	// imports cost 702 MB unbounded, only 2.5x, because the file listing them
	// is itself capped.
	const budget = 96 << 20
	if grew > budget {
		t.Errorf("expanding %d imports of %d KB allocated %d MB; the fan-out is unbounded",
			imports, kb, grew>>20)
	}
}

// Ordinary use: an instruction file split across a handful of imports still
// arrives whole. A budget that stopped a normal AGENTS.md would be worse than
// the fan-out it exists to bound.
func TestOrdinaryImportsStillExpandFully(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("TAPIOCA_CONFIG_DIR", t.TempDir())
	if err := os.MkdirAll(filepath.Join(dir, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	docs := filepath.Join(dir, "docs")
	if err := os.MkdirAll(docs, 0o755); err != nil {
		t.Fatal(err)
	}
	var imports strings.Builder
	imports.WriteString("# Project\n\n")
	for _, name := range []string{"style", "testing", "layout", "review", "release"} {
		if err := os.WriteFile(filepath.Join(docs, name+".md"),
			[]byte("## "+name+"\n"+strings.Repeat("guidance about "+name+".\n", 200)), 0o644); err != nil {
			t.Fatal(err)
		}
		fmt.Fprintf(&imports, "@docs/%s.md\n", name)
	}
	if err := os.WriteFile(filepath.Join(dir, "AGENTS.md"), []byte(imports.String()), 0o644); err != nil {
		t.Fatal(err)
	}

	out := Instructions(dir)
	for _, name := range []string{"style", "testing", "layout", "review", "release"} {
		if !strings.Contains(out, "## "+name) {
			t.Errorf("the %s import was not expanded:\n%s", name, out[:min(400, len(out))])
		}
	}
}
