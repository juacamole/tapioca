package ui

import (
	"bytes"
	"io"
	"os"
)

// A terminal sends the same byte for enter and shift+enter — a bare CR — so
// there is nothing to bind shift+enter to. That is not a gap Tapioca can close
// on its own:
//
//   - Kitty's "disambiguate escape codes" flag does not help. Its
//     specification says outright that "the only exceptions are the Enter, Tab
//     and Backspace keys which still generate the same bytes as in legacy
//     mode". Enter is named as an exception.
//   - The flag that would work is "report all keys as escape codes", which
//     reports *every* key that way, letters included. Bubbletea v1 decodes no
//     CSI-u at all — verified by feeding it both encodings and watching no key
//     event arrive — so turning that on would silence the whole keyboard.
//
// Asking for disambiguation anyway would be worse than doing nothing: it also
// moves ctrl+key and esc onto CSI-u, which Bubbletea would then swallow, so
// keys that work today would stop.
//
// What is left is the half that does work. A terminal can be configured to
// send a distinct sequence for shift+enter, and this rewrites that sequence
// into the byte that already means newline. One line in kitty.conf:
//
//	map shift+enter send_text all \x1b[13;2u
//
// ctrl+j needs none of this and is what the app advertises.

// newlineByte is ctrl+j (LF), which Bubbletea reports as "ctrl+j" and the
// newline action already binds. Translating to it means the rest of the app
// never has to know any of this happened.
const newlineByte = 0x0a

// shiftEnterSequences are the encodings a terminal may be configured to send
// for shift+enter. The first is the Kitty protocol form (CSI 13 ; 2 u); the
// second is xterm's older modifyOtherKeys form.
var shiftEnterSequences = [][]byte{
	[]byte("\x1b[13;2u"),
	[]byte("\x1b[27;2;13~"),
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
