package ui

import (
	"bytes"
	"io"
	"os"
	"testing"
)

func readAll(t *testing.T, r io.Reader, chunk int) []byte {
	t.Helper()
	var out []byte
	buf := make([]byte, chunk)
	for {
		n, err := r.Read(buf)
		out = append(out, buf[:n]...)
		if err == io.EOF {
			return out
		}
		if err != nil {
			t.Fatal(err)
		}
	}
}

func TestShiftEnterBecomesANewline(t *testing.T) {
	for _, seq := range shiftEnterSequences {
		in := append(append([]byte("ab"), seq...), []byte("cd")...)
		got := readAll(t, ExtendedKeyReader(bytes.NewReader(in)), 64)
		want := []byte{'a', 'b', newlineByte, 'c', 'd'}
		if !bytes.Equal(got, want) {
			t.Errorf("%q -> %q, want %q", in, got, want)
		}
	}
}

// A sequence can straddle two reads. Emitting the first half would put a stray
// escape into the prompt and lose the newline.
func TestSequenceSplitAcrossReadsStillTranslates(t *testing.T) {
	seq := shiftEnterSequences[0]
	for cut := 1; cut < len(seq); cut++ {
		r := ExtendedKeyReader(io.MultiReader(
			bytes.NewReader(seq[:cut]),
			bytes.NewReader(seq[cut:]),
		))
		got := readAll(t, r, 64)
		if !bytes.Equal(got, []byte{newlineByte}) {
			t.Errorf("split at %d -> %q, want a single newline", cut, got)
		}
	}
}

// Everything that is not shift+enter has to pass through byte for byte —
// including a plain carriage return, which is how a prompt gets sent.
func TestOtherInputIsUntouched(t *testing.T) {
	for _, in := range []string{
		"hello world",
		"\r",                // enter
		"\n",                // ctrl+j
		"\x1b[A",            // up arrow
		"\x1b[13;5u",        // ctrl+enter, a different modifier
		"\x1b[27;2;9~",      // shift+tab in the modifyOtherKeys form
		"\x1b",              // a lone escape
		"\x1b[<0;10;5M",     // a mouse report
		"line\rnext\x1b[Bx", // mixed traffic
	} {
		got := readAll(t, ExtendedKeyReader(bytes.NewReader([]byte(in))), 64)
		if !bytes.Equal(got, []byte(in)) {
			t.Errorf("%q was rewritten to %q", in, got)
		}
	}
}

// A truncated sequence at end of stream must still be delivered rather than
// swallowed, or input is silently lost.
func TestTruncatedSequenceIsNotSwallowed(t *testing.T) {
	partial := "\x1b[13;"
	got := readAll(t, ExtendedKeyReader(bytes.NewReader([]byte(partial))), 64)
	if !bytes.Equal(got, []byte(partial)) {
		t.Errorf("got %q, want the partial sequence %q back", got, partial)
	}
}

// The caller's buffer bounds every read; a translation must not overflow it or
// drop the remainder.
func TestSmallReadBufferLosesNothing(t *testing.T) {
	seq := shiftEnterSequences[0]
	in := append(append([]byte("abc"), seq...), []byte("defghij")...)
	for _, chunk := range []int{1, 2, 3, 5} {
		got := readAll(t, ExtendedKeyReader(bytes.NewReader(in)), chunk)
		want := append(append([]byte("abc"), newlineByte), []byte("defghij")...)
		if !bytes.Equal(got, want) {
			t.Errorf("chunk %d -> %q, want %q", chunk, got, want)
		}
	}
}

// Bubbletea enters raw mode only when its input satisfies term.File. If the
// wrapper hides that, every keystroke waits for enter and the whole TUI is
// unusable — silently, since nothing reports the missing raw mode.
func TestWrappedStdinStillLooksLikeATerminal(t *testing.T) {
	r := ExtendedKeyReader(os.Stdin)
	f, ok := r.(interface {
		io.ReadWriteCloser
		Fd() uintptr
	})
	if !ok {
		t.Fatal("the wrapped input does not satisfy term.File — bubbletea would not enter raw mode")
	}
	if f.Fd() != os.Stdin.Fd() {
		t.Errorf("Fd() = %d, want stdin's %d", f.Fd(), os.Stdin.Fd())
	}
}

// Closing the wrapper must not close stdin underneath the program.
func TestCloseDoesNotCloseStdin(t *testing.T) {
	r := ExtendedKeyReader(os.Stdin).(io.ReadWriteCloser)
	if err := r.Close(); err != nil {
		t.Fatalf("Close() = %v", err)
	}
	if _, err := os.Stdin.Stat(); err != nil {
		t.Errorf("stdin was closed: %v", err)
	}
}
