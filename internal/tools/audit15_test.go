package tools

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

// bigText writes n bytes of ordinary repeated text. A repository ships this for
// nothing: 64 MiB of one line deflates to a few kilobytes in a git object, and
// a tarball can store it as a sparse member that occupies no blocks at all.
func bigText(t *testing.T, path string, n int) {
	t.Helper()
	block := bytes.Repeat([]byte("the quick brown fox jumps over the lazy dog\n"), 1500)
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	for w := 0; w < n; w += len(block) {
		if _, err := f.Write(block); err != nil {
			f.Close()
			t.Fatal(err)
		}
	}
	// One line that appears exactly once, so edit_file has something to match
	// without tripping the "appears N times" refusal first.
	if _, err := f.WriteString("ONE-OF-A-KIND\n"); err != nil {
		f.Close()
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

// peakHeap reports the largest live heap seen while fn ran. Live heap is what
// decides whether the process is killed; total allocation is not, since a
// stream that allocates and drops one line at a time never holds any of it.
func peakHeap(fn func()) uint64 {
	stop, stopped := make(chan struct{}), make(chan uint64)
	go func() {
		var ms runtime.MemStats
		var peak uint64
		for {
			runtime.ReadMemStats(&ms)
			if ms.HeapAlloc > peak {
				peak = ms.HeapAlloc
			}
			select {
			case <-stop:
				stopped <- peak
				return
			default:
			}
			time.Sleep(200 * time.Microsecond)
		}
	}()
	runtime.GC()
	fn()
	close(stop)
	return <-stopped
}

// read_file caps its read at 16 MiB and writes down why: a file the process
// cannot hold kills the TUI and everything since the last save goes with it.
// The other three tools that open a file the repository chose read all of it.
//
// grep is the worst of them, because it is the one that never reaches the
// permission gate — it is non-mutating, so it runs unprompted in every mode
// including plan mode. One grep over a tree holding a single 64 MiB file held
// 160 MB live, and the file's size is the repository's to pick, so the same
// walk over a 1 GiB file holds 2.5 GB. write_file and edit_file read their
// target the same way before touching it.
func TestUnboundedReadsInSearchAndEditPaths(t *testing.T) {
	const size = 64 << 20 // four times maxReadBytes, so the two cannot be confused
	const budget = 96 << 20

	ctx := context.Background()
	var asked []string
	allow := asker(Decision{Allow: true}, &asked)
	// Each case gets its own executor and its own copy of the file: the tools
	// record what they have read, and a file rewritten under one of them is
	// refused as stale before it is opened at all.
	setup := func(t *testing.T) *Executor {
		e := execIn(t, ModeAuto)
		bigText(t, filepath.Join(e.Cwd(), "blob.txt"), size)
		return e
	}

	for _, tc := range []struct {
		name    string
		control bool
		call    func(*Executor)
	}{
		// Control: read_file's own cap holds, so a peak under the budget for
		// the others means a cap held rather than the measurement being blind.
		{"read_file", true, func(e *Executor) {
			if _, _, err := e.Call(ctx, "read_file",
				args(t, map[string]any{"path": "blob.txt", "limit": 5}), allow); err != nil {
				t.Error(err)
			}
		}},
		{"grep", false, func(e *Executor) {
			// The Go walk, not ripgrep: rg streams and bounds itself, so the
			// machines that reach this code are the ones without it — CI, and
			// every user who never installed it.
			if _, _, err := e.grepWalk(ctx, "needle", e.Cwd(), "", false, 10); err != nil {
				t.Error(err)
			}
		}},
		{"write_file", false, func(e *Executor) {
			if _, _, err := e.Call(ctx, "write_file",
				args(t, map[string]any{"path": "blob.txt", "content": "x"}), allow); err != nil {
				t.Error(err)
			}
		}},
		{"edit_file", false, func(e *Executor) {
			// The marker bigText puts at the end appears once, so this is the
			// path that reads the file and writes it back, not an early error.
			if _, _, err := e.Call(ctx, "edit_file",
				args(t, map[string]any{"path": "blob.txt", "old_string": "ONE-OF-A-KIND", "new_string": "x"}), allow); err != nil {
				t.Error(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := setup(t)
			got := peakHeap(func() { tc.call(e) })
			t.Logf("%s peak live heap: %d bytes", tc.name, got)
			if got > budget && !tc.control {
				t.Errorf("%s held %d bytes live for a %d byte file whose size the repository chose; read_file caps the same read at %d",
					tc.name, got, size, maxReadBytes)
			}
			if got > budget && tc.control {
				t.Errorf("control: read_file held %d bytes live for a %d byte file though its own cap is %d; this test cannot tell a cap from its absence",
					got, size, maxReadBytes)
			}
		})
	}
}

// A FileChange is display-only, but it is kept in the transcript for the rest
// of the session, and diff emits one Op per line on both sides — unchanged
// lines included. Overwriting a 15 MiB file (under read_file's cap, so nothing
// above refuses the read) built 358,502 Ops and held 20 MB of them, for a
// display that shows forty lines and a header of two numbers.
func TestADisplayOnlyDiffIsNotKeptAtWholeFileScale(t *testing.T) {
	e := execIn(t, ModeAuto)
	bigText(t, filepath.Join(e.Cwd(), "blob.txt"), 15<<20)
	var asked []string
	allow := asker(Decision{Allow: true}, &asked)

	res, err := e.CallDetailed(context.Background(), "write_file",
		args(t, map[string]any{"path": "blob.txt", "content": "x\n"}), allow)
	if err != nil {
		t.Fatal(err)
	}
	// Control: the write happened and a diff was recorded, so an empty op list
	// below would be this test measuring nothing.
	if res.Change == nil {
		t.Fatalf("control: no change recorded for an overwrite: %q", res.Text)
	}
	if res.Change.Removed < 100000 {
		t.Fatalf("control: the header reports only %d lines removed, so the file was not read whole", res.Change.Removed)
	}

	t.Logf("ops kept: %d for %d removed and %d added", len(res.Change.Ops), res.Change.Removed, res.Change.Added)
	if len(res.Change.Ops) > 10*maxKeptOps {
		t.Errorf("a display-only diff kept %d ops (%d bytes or so) for a transcript that shows %d lines",
			len(res.Change.Ops), len(res.Change.Ops)*56, diffMaxLines)
	}
	// The counts in the header are what the user reads; capping the list must
	// not turn them into a count of what survived the cap.
	if got := remaining(res.Change, diffMaxLines); got != res.Change.Added+res.Change.Removed-diffMaxLines {
		t.Errorf("the transcript reports %d more lines, of %d changed", got, res.Change.Added+res.Change.Removed)
	}
}
