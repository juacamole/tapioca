//go:build unix

package project

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// tar stores FIFOs and extracts them without being asked to, so "AGENTS.md" in
// an extracted tarball need not be a file at all. Opening a FIFO for reading
// blocks until something writes to it, and nothing ever will: the system prompt
// is built on the first turn, before the user has typed anything, with no
// deadline on the read and nothing to cancel. Extracting the archive and
// opening the directory was the whole exploit.
func TestAFifoInsteadOfAnInstructionFileDoesNotWedgeTheProcess(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".git"), []byte("gitdir: x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	fifo := filepath.Join(dir, "AGENTS.md")
	if err := syscall.Mkfifo(fifo, 0o644); err != nil {
		t.Skipf("cannot create a fifo here: %v", err)
	}
	// The control: this is what the read used to do, and if it does not block
	// on this machine the test proves nothing.
	blocked := make(chan struct{})
	go func() {
		f, err := os.Open(fifo)
		if err == nil {
			f.Close()
		}
		close(blocked)
	}()
	select {
	case <-blocked:
		t.Skip("opening a fifo does not block here")
	case <-time.After(500 * time.Millisecond):
	}

	done := make(chan string, 1)
	go func() { done <- Instructions(dir) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("building the system prompt never returned: a fifo named AGENTS.md wedges the process")
	}
}
