package ui

import (
	"bytes"
	"io"
	"os"
)

// shift+enter is the documented way to write a second line, and for a long
// time it did nothing. The reason is that a terminal sends the same byte for
// enter and shift+enter — a bare CR — so there is nothing to bind to. Modern
// terminals will encode the modifier, but only if the application asks, and
// Bubbletea v1 neither asks nor decodes the result.
//
// So both halves are done here: ask the terminal to disambiguate, then rewrite
// the sequence it sends into the one byte that already means "newline" on the
// wire. Anything else keeps working untouched, and a terminal that ignores the
// request is no worse off than before — ctrl+j still inserts a newline.

const (
	// Push the Kitty keyboard protocol's "disambiguate escape codes" flag.
	// Supported by kitty, ghostty, foot, WezTerm and recent iTerm2; ignored by
	// terminals that do not know it, which is why this is safe to send blind.
	extKeysEnable = "\x1b[>1u"
	// Pop it again. Leaving it pushed would hand the user's shell a keyboard
	// encoding it did not ask for, so this must run even when the app dies.
	extKeysDisable = "\x1b[<u"

	// newlineByte is ctrl+j (LF), which Bubbletea reports as "ctrl+j" and the
	// newline action already binds. Translating to it means the rest of the
	// app never has to know any of this happened.
	newlineByte = 0x0a
)

// shiftEnterSequences are the encodings a terminal may use for shift+enter.
// The first is the Kitty protocol form (CSI 13 ; 2 u); the second is xterm's
// older modifyOtherKeys form, which some terminals send instead.
var shiftEnterSequences = [][]byte{
	[]byte("\x1b[13;2u"),
	[]byte("\x1b[27;2;13~"),
}

// EnableExtendedKeys asks the terminal to distinguish shift+enter from enter
// and returns the function that undoes it. The caller must defer the result:
// the flag lives in the terminal, not the process, and survives a crash.
func EnableExtendedKeys() func() {
	if os.Getenv("TAPIOCA_NO_EXTENDED_KEYS") != "" {
		return func() {}
	}
	_, _ = os.Stdout.WriteString(extKeysEnable)
	return func() { _, _ = os.Stdout.WriteString(extKeysDisable) }
}

// extKeyReader rewrites shift+enter sequences into a plain newline byte as
// input is read.
//
// Fd, Write and Close are forwarded rather than merely present: Bubbletea puts
// the terminal into raw mode only when its input satisfies term.File, which is
// io.ReadWriteCloser plus Fd. Wrapping stdin in something that hides those
// would leave the whole TUI line-buffered, waiting for enter before it saw a
// single keystroke — a far worse bug than the one being fixed, and a silent
// one, since nothing reports the missing raw mode.
type extKeyReader struct {
	src io.Reader
	tty *os.File // nil when the input is not a terminal (tests, pipes)
	buf []byte   // a sequence split across two reads
}

// ExtendedKeyReader wraps the program's input so shift+enter arrives as
// something bindable, preserving the terminal-ness of the underlying stream.
func ExtendedKeyReader(r io.Reader) io.Reader {
	e := &extKeyReader{src: r}
	if f, ok := r.(*os.File); ok {
		e.tty = f
	}
	return e
}

func (e *extKeyReader) Fd() uintptr {
	if e.tty == nil {
		return 0
	}
	return e.tty.Fd()
}

func (e *extKeyReader) Write(p []byte) (int, error) {
	if e.tty == nil {
		return 0, io.ErrClosedPipe
	}
	return e.tty.Write(p)
}

// Close does not close the wrapped file: stdin is not this type's to close,
// and doing so would take the terminal down with it.
func (e *extKeyReader) Close() error { return nil }

// longestSequence is how many bytes must be held back when input ends
// mid-sequence.
func longestSequence() int {
	n := 0
	for _, s := range shiftEnterSequences {
		if len(s) > n {
			n = len(s)
		}
	}
	return n
}

func (e *extKeyReader) Read(p []byte) (int, error) {
	scratch := make([]byte, len(p))
	n, err := e.src.Read(scratch)
	data := append(e.buf, scratch[:n]...)
	e.buf = nil

	for _, seq := range shiftEnterSequences {
		data = bytes.ReplaceAll(data, seq, []byte{newlineByte})
	}

	// A read can end part-way through a sequence, and emitting that prefix
	// would put a stray escape in the prompt. Hold back anything that could
	// still become one, unless the stream is over and it never will.
	if err == nil {
		if keep := danglingPrefix(data); keep > 0 {
			e.buf = append([]byte(nil), data[len(data)-keep:]...)
			data = data[:len(data)-keep]
		}
	}

	// Never overflow the caller's buffer: keep the remainder for next time.
	if len(data) > len(p) {
		e.buf = append(append([]byte(nil), data[len(p):]...), e.buf...)
		data = data[:len(p)]
	}
	copy(p, data)
	if len(data) > 0 {
		return len(data), nil // surface any error on the next call
	}
	return 0, err
}

// danglingPrefix returns how many trailing bytes are a proper prefix of some
// sequence, and so cannot be judged yet.
func danglingPrefix(data []byte) int {
	maxKeep := min(longestSequence()-1, len(data))
	for keep := maxKeep; keep > 0; keep-- {
		tail := data[len(data)-keep:]
		for _, seq := range shiftEnterSequences {
			if len(tail) < len(seq) && bytes.Equal(tail, seq[:len(tail)]) {
				return keep
			}
		}
	}
	return 0
}
