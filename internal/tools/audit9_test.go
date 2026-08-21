package tools

import (
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"
)

// web_fetch needs no approval in any mode; the only thing in front of it is
// the prompt the first time a host is fetched. That prompt was keyed on a host
// worked out by rules that were not the rules the fetch itself used: the gate
// prepended a scheme only when the argument held no "://" *anywhere*, and read
// the argument without trimming it, while the fetch trimmed and looked only at
// the start. Every spelling below made the gate report no host at all — and no
// host means no prompt — while the fetch went to the attacker's.
func TestEveryFetchSpellingIsGatedOnTheHostItReaches(t *testing.T) {
	// The rule the gate used to apply, kept as the control: a test that only
	// asserts the current answer proves nothing about what was wrong with the
	// old one.
	looseHost := func(raw string) string {
		if !strings.Contains(raw, "://") {
			raw = "https://" + raw
		}
		u, err := url.Parse(raw)
		if err != nil {
			return ""
		}
		return strings.ToLower(u.Hostname())
	}
	for _, raw := range []string{
		"evil.example/x?u=https://good.example", // a "://" later in the string
		" https://evil.example/x",               // leading space
		"\thttps://evil.example/x",              // leading tab
		"evil.example/a://b",                    // a "://" in the path
	} {
		if looseHost(raw) == "evil.example" {
			t.Errorf("%q: the old rule already read this correctly, so it proves nothing", raw)
		}
		if got := FetchHost(raw); got != "evil.example" {
			t.Errorf("%q: the gate would ask about %q, and the fetch goes to evil.example", raw, got)
		}
	}
	// The same disagreement the other way round: a scheme is case-insensitive
	// to the gate's url.Parse and was not to the fetch, so "HTTP://x" was
	// prompted for as x and then requested as the nonsense "https://HTTP://x".
	if got := fetchTarget("HTTP://evil.example/x"); got != "HTTP://evil.example/x" {
		t.Errorf("an upper-case scheme became %q", got)
	}
	if got := FetchHost("HTTP://evil.example/x"); got != "evil.example" {
		t.Errorf("an upper-case scheme is gated on %q", got)
	}
	// Ordinary spellings are unchanged.
	for raw, want := range map[string]string{
		"https://good.example/x":      "good.example",
		"good.example/x":              "good.example",
		"http://good.example:8080/x":  "good.example",
		"https://Good.Example/x":      "good.example",
		"https://u:p@good.example/x":  "good.example",
		"https://[::1]:9000/x":        "::1",
		"not a url at all":            "",
		"":                            "",
		"https://good.example?q=a://": "good.example",
	} {
		if got := FetchHost(raw); got != want {
			t.Errorf("FetchHost(%q) = %q, want %q", raw, got, want)
		}
	}
}

// A file in an extracted tarball need not be a file: tar stores FIFOs and
// extracts them without being asked to. Opening one for reading blocks until
// something writes to it, and nothing here ever will — no deadline reaches
// os.Open, and the call's own context is not consulted by it, so the turn hung
// with the tool call never returning.
func TestAFifoDoesNotWedgeAFileTool(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no mkfifo on windows")
	}
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

// Refusing what is not a regular file must not refuse the files a coding agent
// actually reads. /proc and /sys entries report a size of zero and are regular;
// so is every file in a project.
func TestOrdinaryFilesAreStillReadable(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	e := NewExecutor(dir, ModeBypass)
	allow := func(string, string) Decision { return Decision{Allow: true} }

	raw, _ := json.Marshal(map[string]any{"path": "main.go"})
	if out, isErr, _ := e.Call(t.Context(), "read_file", raw, allow); isErr {
		t.Errorf("read_file on an ordinary file failed: %s", out)
	}
	raw, _ = json.Marshal(map[string]any{"path": "new.go", "content": "package main\n"})
	if out, isErr, _ := e.Call(t.Context(), "write_file", raw, allow); isErr {
		t.Errorf("write_file creating a file failed: %s", out)
	}
	raw, _ = json.Marshal(map[string]any{"path": "main.go", "old_string": "main", "new_string": "x", "replace_all": true})
	if out, isErr, _ := e.Call(t.Context(), "edit_file", raw, allow); isErr {
		t.Errorf("edit_file on an ordinary file failed: %s", out)
	}
	if runtime.GOOS == "linux" {
		raw, _ = json.Marshal(map[string]any{"path": "/proc/self/status"})
		if out, isErr, _ := e.Call(t.Context(), "read_file", raw, allow); isErr {
			t.Errorf("read_file on /proc/self/status failed: %s", out)
		}
	}
}

// The gate and the fetch have to agree on the whole URL, not only its host: a
// prompt naming one host and a request to another is the same defect either
// way round. This drives the real fetch against a test server and checks that
// the host the gate would have asked about is the host that was contacted.
func TestTheGatedHostIsTheHostContacted(t *testing.T) {
	var gotHost string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHost, _, _ = net.SplitHostPort(r.Host)
		w.Write([]byte("ok"))
	}))
	defer srv.Close()

	e := NewExecutor(t.TempDir(), ModeBypass)
	for _, raw := range []string{
		srv.URL + "/x?u=https://good.example",
		" " + srv.URL + "/x",
		strings.ToUpper("HTTP") + strings.TrimPrefix(srv.URL, "http") + "/x",
	} {
		gotHost = ""
		args, _ := json.Marshal(map[string]string{"url": raw})
		if _, isErr, _ := e.webFetch(t.Context(), args); isErr {
			t.Fatalf("%q: fetch failed", raw)
		}
		if gotHost == "" {
			t.Fatalf("%q: the server was never reached", raw)
		}
		if want := FetchHost(raw); !strings.EqualFold(gotHost, want) {
			t.Errorf("%q: gated on %q, contacted %q", raw, want, gotHost)
		}
	}
}
