package lsp

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"tapioca/internal/config"
)

func TestFormatSortsErrorsFirst(t *testing.T) {
	out := Format([]Diagnostic{
		{Line: 9, Severity: SeverityWarning, Message: "unused variable"},
		{Line: 3, Severity: SeverityError, Message: "undefined: foo"},
		{Line: 1, Severity: SeverityHint, Message: "consider simplifying"},
	})
	if !strings.HasPrefix(out, "diagnostics: 3 problem(s), 1 error(s)") {
		t.Fatalf("summary line wrong:\n%s", out)
	}
	lines := strings.Split(out, "\n")
	if !strings.Contains(lines[1], "undefined: foo") {
		t.Errorf("errors should come first:\n%s", out)
	}
	if Format(nil) != "" {
		t.Error("a clean file should produce no note")
	}
}

func TestFormatCapsTheList(t *testing.T) {
	var many []Diagnostic
	for i := 0; i < maxShown+5; i++ {
		many = append(many, Diagnostic{Line: i + 1, Severity: SeverityError, Message: "boom"})
	}
	out := Format(many)
	if strings.Count(out, "boom") != maxShown {
		t.Errorf("expected %d shown, got %d", maxShown, strings.Count(out, "boom"))
	}
	if !strings.Contains(out, "… 5 more") {
		t.Errorf("truncation not reported:\n%s", out)
	}
}

func TestHandlesMatchesExtensions(t *testing.T) {
	c := &Client{Exts: []string{".go", ".MOD"}}
	for _, ok := range []string{"/a/b/main.go", "/a/go.MOD", "/a/go.mod"} {
		if !c.Handles(ok) {
			t.Errorf("should handle %s", ok)
		}
	}
	if c.Handles("/a/b/main.rs") {
		t.Error("should not handle .rs")
	}
}

func TestPathToURI(t *testing.T) {
	if got := pathToURI("/tmp/a b/main.go"); got != "file:///tmp/a%20b/main.go" {
		t.Errorf("uri = %q", got)
	}
}

// The point of the whole package: an edit that breaks the code comes back with
// the compiler's complaint attached, without waiting for a build.
func TestGoplsReportsABrokenEdit(t *testing.T) {
	if _, err := exec.LookPath("gopls"); err != nil {
		t.Skip("gopls not installed")
	}
	root := t.TempDir()
	write := func(name, body string) string {
		p := filepath.Join(root, name)
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	write("go.mod", "module example.com/probe\n\ngo 1.21\n")
	main := write("main.go", "package main\n\nfunc main() {\n\tprintln(\"ok\")\n}\n")

	r := NewRegistry([]config.LSPServerConfig{{
		Name: "gopls", Command: "gopls", Extensions: []string{".go"},
	}}, root)
	defer r.CloseAll()

	// A cold server is still loading the workspace, so give the first check
	// room; this is the cost the real flow pays once per session.
	deadline := time.Now().Add(60 * time.Second)
	var note string
	for time.Now().Before(deadline) {
		write("main.go", "package main\n\nfunc main() {\n\tundefinedFunction()\n}\n")
		note = r.Check(main)
		if strings.Contains(note, "undefined") {
			break
		}
		time.Sleep(2 * time.Second)
	}
	if !strings.Contains(note, "undefined") {
		t.Fatalf("gopls did not report the broken call; got:\n%s", note)
	}
	if !strings.Contains(note, "diagnostics:") {
		t.Errorf("note is not formatted for the model:\n%s", note)
	}

	// And a correct file should come back clean.
	write("main.go", "package main\n\nfunc main() {\n\tprintln(\"fine\")\n}\n")
	for i := 0; i < 10; i++ {
		if note = r.Check(main); note == "" {
			break
		}
		time.Sleep(time.Second)
	}
	if note != "" {
		t.Errorf("a valid file still reported problems:\n%s", note)
	}
}

func TestMissingServerIsRecordedNotRetriedForever(t *testing.T) {
	r := NewRegistry([]config.LSPServerConfig{{
		Name: "nope", Command: "definitely-not-a-real-language-server", Extensions: []string{".go"},
	}}, t.TempDir())
	if note := r.Check(filepath.Join(t.TempDir(), "x.go")); note != "" {
		t.Errorf("a missing server should be silent, got %q", note)
	}
	if len(r.Errors()) != 1 {
		t.Errorf("the failure was not recorded: %v", r.Errors())
	}
}

// A server that is still loading publishes placeholders about itself, not
// about the edit; passing those to the model sends it hunting for a problem
// that does not exist.
func TestWorkspaceLoadingMessagesAreSuppressed(t *testing.T) {
	loading := []Diagnostic{
		{Line: 1, Severity: SeverityError, Message: "No active builds contain this file", Source: "gopls"},
	}
	if !allNotReady(loading) {
		t.Error("startup placeholder not recognized")
	}
	real := []Diagnostic{
		{Line: 4, Severity: SeverityError, Message: "undefined: doesNotExist", Source: "compiler"},
	}
	if allNotReady(real) {
		t.Error("a real diagnostic was mistaken for a startup placeholder")
	}
	// A mix means the server is up and has something to say.
	if allNotReady(append(append([]Diagnostic{}, loading...), real...)) {
		t.Error("a real diagnostic alongside a placeholder was suppressed")
	}
	if allNotReady(nil) {
		t.Error("no diagnostics is a clean file, not a loading server")
	}
}
