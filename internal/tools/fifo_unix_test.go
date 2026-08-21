//go:build unix

package tools

import (
	"encoding/json"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// A file in an extracted tarball need not be a file: tar stores FIFOs and
// extracts them without being asked to. Opening one for reading blocks until
// something writes to it, and nothing here ever will — no deadline reaches
// os.Open, and the call's own context is not consulted by it, so the turn hung
// with the tool call never returning.
func TestAFifoDoesNotWedgeAFileTool(t *testing.T) {
	dir := t.TempDir()
	fifo := filepath.Join(dir, "notes.md")
	if err := syscall.Mkfifo(fifo, 0o644); err != nil {
		t.Skipf("cannot create a fifo here: %v", err)
	}
	// The control: this is what each tool used to do, and if it does not block
	// on this machine the test proves nothing.
	opened := make(chan struct{})
	go func() {
		if f, err := os.Open(fifo); err == nil {
			f.Close()
		}
		close(opened)
	}()
	select {
	case <-opened:
		t.Skip("opening a fifo does not block here")
	case <-time.After(500 * time.Millisecond):
	}

	e := NewExecutor(dir, ModeBypass)
	for _, c := range []struct {
		tool string
		args map[string]any
	}{
		{"read_file", map[string]any{"path": "notes.md"}},
		{"write_file", map[string]any{"path": "notes.md", "content": "x"}},
		{"edit_file", map[string]any{"path": "notes.md", "old_string": "a", "new_string": "b"}},
	} {
		raw, _ := json.Marshal(c.args)
		done := make(chan bool, 1)
		go func() {
			_, isErr, _ := e.Call(t.Context(), c.tool, raw, func(string, string) Decision {
				return Decision{Allow: true}
			})
			done <- isErr
		}()
		select {
		case isErr := <-done:
			if !isErr {
				t.Errorf("%s on a fifo reported success", c.tool)
			}
		case <-time.After(5 * time.Second):
			t.Errorf("%s on a fifo never returned", c.tool)
		}
	}
}
