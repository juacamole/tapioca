package lsp_test

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"tapioca/internal/config"
	"tapioca/internal/lsp"
	"tapioca/internal/tools"
)

// The whole point is that the model learns about a broken edit from the tool
// result. This asserts that end of the wiring, without a model in the loop to
// paraphrase it away.
func TestBrokenEditComesBackWithDiagnostics(t *testing.T) {
	if _, err := exec.LookPath("gopls"); err != nil {
		t.Skip("gopls not installed")
	}
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte("module ex\n\ngo 1.21\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	main := filepath.Join(root, "main.go")
	if err := os.WriteFile(main, []byte("package main\n\nfunc main() {\n\tprintln(\"ok\")\n}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	reg := lsp.NewRegistry([]config.LSPServerConfig{{
		Name: "gopls", Command: "gopls", Extensions: []string{".go"},
	}}, root)
	defer reg.CloseAll()
	reg.Warm() // main.go warms servers at startup; mirror that here

	e := tools.NewExecutor(root, tools.ModeBypass)
	e.SetDiagnostics(reg.Check)
	allow := func(string, string) tools.Decision { return tools.Decision{Allow: true} }

	edit := func(from, to string) string {
		t.Helper()
		raw, _ := json.Marshal(map[string]any{"path": "main.go", "old_string": from, "new_string": to})
		res, err := e.CallDetailed(context.Background(), "edit_file", raw, allow)
		if err != nil {
			t.Fatal(err)
		}
		return res.Text
	}

	// The first edit may land while the server is still loading, which is
	// reported as nothing at all rather than as a fake problem. Retry until
	// the workspace is up.
	var out string
	deadline := time.Now().Add(60 * time.Second)
	for i := 0; time.Now().Before(deadline); i++ {
		if i%2 == 0 {
			out = edit("println(\"ok\")", "doesNotExist()")
		} else {
			out = edit("doesNotExist()", "println(\"ok\")")
		}
		if strings.Contains(out, "undefined") {
			break
		}
		time.Sleep(time.Second)
	}
	if !strings.Contains(out, "diagnostics:") || !strings.Contains(out, "undefined") {
		t.Fatalf("the edit result carried no diagnostics:\n%s", out)
	}
	if !strings.Contains(out, "edited") {
		t.Errorf("the normal result was lost:\n%s", out)
	}
	t.Logf("tool result the model receives:\n%s", out)
}
