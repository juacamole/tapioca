package lsp

import (
	"bufio"
	"strings"
	"testing"
)

// A language server is a subprocess that can be wrong or hostile, and both
// look the same from here. Content-Length went from Atoi straight into a
// make(), on a goroutine with no recover — so one header line killed Tapioca.
func TestFrameRejectsAbsurdContentLength(t *testing.T) {
	cases := []string{
		"Content-Length: 9223372036854775807\r\n\r\n",
		"Content-Length: 1500000000\r\n\r\n",
		"Content-Length: 99999999999999999999\r\n\r\n", // overflows Atoi
	}
	for _, header := range cases {
		r := bufio.NewReader(strings.NewReader(header))
		body, err := readFrame(r)
		if err == nil {
			t.Errorf("%q was accepted, returning %d bytes", strings.TrimSpace(header), len(body))
		}
	}
}

// The header loop read a line with no bound, so a server that never sends a
// newline could make us buffer without end.
func TestFrameRejectsAnEndlessHeaderLine(t *testing.T) {
	huge := "Content-Length: " + strings.Repeat("0", 64<<10)
	r := bufio.NewReader(strings.NewReader(huge))
	if _, err := readFrame(r); err == nil {
		t.Fatal("an unterminated header line of 64KB was accepted")
	}
}

// Ordinary framing must still work, or the cap is just a break.
func TestFrameReadsAnOrdinaryMessage(t *testing.T) {
	body := `{"jsonrpc":"2.0","id":1,"result":{}}`
	msg := "Content-Length: 36\r\n\r\n" + body
	r := bufio.NewReader(strings.NewReader(msg))
	got, err := readFrame(r)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != body {
		t.Fatalf("got %q, want %q", got, body)
	}
}
