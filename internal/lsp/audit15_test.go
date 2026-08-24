package lsp

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// Every bound in this package counts things: frames, header bytes, tracked
// files, and the twenty problems Format prints. None of them bounded how long
// one message is, or how many a single publish may carry — and a diagnostic
// message is the server's prose about bytes the repository wrote, quoted back
// inside the complaint about them. What comes out the other end is what
// edit_file appends to its result, which is what the model is sent, what the
// provider is billed for, and what the transcript keeps for the session.
func TestOneDiagnosticsPublishCannotFillTheTranscript(t *testing.T) {
	const long = 4 << 20
	const many = 200000

	// Control: an ordinary message survives intact, so a short result below is
	// the clipping and not the whole path going missing.
	c := &Client{diags: map[string][]Diagnostic{}, fresh: map[string]chan struct{}{}}
	c.handleDiagnostics(publish(t, "file:///x.go", []string{"undefined: foo"}))
	if got := Format(c.diags["file:///x.go"]); !strings.Contains(got, "undefined: foo") {
		t.Fatalf("control: an ordinary diagnostic did not survive: %q", got)
	}

	t.Run("one enormous message", func(t *testing.T) {
		c := &Client{diags: map[string][]Diagnostic{}, fresh: map[string]chan struct{}{}}
		c.handleDiagnostics(publish(t, "file:///x.go", []string{
			"cannot use \"" + strings.Repeat("A", long) + "\" as int value",
		}))
		out := Format(c.diags["file:///x.go"])
		t.Logf("formatted note: %d bytes from a %d byte message", len(out), long)
		if len(out) > 64<<10 {
			t.Errorf("a single diagnostic put %d bytes into the tool result the model is sent", len(out))
		}
	})

	t.Run("one publish with too many", func(t *testing.T) {
		msgs := make([]string, many)
		for i := range msgs {
			msgs[i] = "undefined: foo"
		}
		c := &Client{diags: map[string][]Diagnostic{}, fresh: map[string]chan struct{}{}}
		c.handleDiagnostics(publish(t, "file:///x.go", msgs))
		kept := len(c.diags["file:///x.go"])
		t.Logf("kept %d of %d published problems", kept, many)
		if kept > 10*maxStoredDiags {
			t.Errorf("%d problems are held for one file, of which Format prints %d", kept, maxShown)
		}
	})
}

func publish(t *testing.T, uri string, messages []string) json.RawMessage {
	t.Helper()
	type rng struct {
		Start struct {
			Line      int `json:"line"`
			Character int `json:"character"`
		} `json:"start"`
	}
	type diag struct {
		Range    rng    `json:"range"`
		Severity int    `json:"severity"`
		Message  string `json:"message"`
		Source   string `json:"source"`
	}
	var ds []diag
	for i, m := range messages {
		d := diag{Severity: SeverityError, Message: m, Source: "srv"}
		d.Range.Start.Line = i
		ds = append(ds, d)
	}
	data, err := json.Marshal(map[string]any{"uri": uri, "diagnostics": ds})
	if err != nil {
		t.Fatal(fmt.Errorf("building publish: %w", err))
	}
	return data
}
