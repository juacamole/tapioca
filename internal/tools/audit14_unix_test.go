//go:build unix

package tools

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The user is not an attacker but can be tricked into approving something whose
// displayed summary does not match what runs. This is that, for the two tools
// that write.
//
// gateReadOnly resolves the path before showing it — read_file and grep both
// prompt with e.resolve(a.Path), so a link is named by its target and the user
// sees the file that will actually be opened. The mutating gate a few hundred
// lines below asks the same question and answers it differently: `outside` is
// decided on e.resolve(argPath(raw)), writeFile writes to e.resolve(a.Path),
// and summary() hands the prompt a.Path exactly as the model wrote it.
//
// git stores symlinks and tar carries them, so a clone can ship
// notes.md -> ~/.ssh/authorized_keys. The model asks to write notes.md, the box
// says notes.md, and the file that changes is authorized_keys. The tool label
// does say "(outside the working directory)", which is worth something — but
// the one line in the box that is supposed to say *what* is being written names
// a file in the project.
func TestTheWritePromptNamesTheFileThatWillBeWritten(t *testing.T) {
	for _, tc := range []struct {
		tool string
		args map[string]string
	}{
		{"write_file", map[string]string{"path": "notes.md", "content": "ssh-rsa AAAA attacker"}},
		{"edit_file", map[string]string{"path": "notes.md", "old_string": "a", "new_string": "b"}},
	} {
		t.Run(tc.tool, func(t *testing.T) {
			e := execIn(t, ModeAuto)

			outside := t.TempDir()
			if resolved, err := filepath.EvalSymlinks(outside); err == nil {
				outside = resolved
			}
			target := filepath.Join(outside, "authorized_keys")
			if err := os.WriteFile(target, []byte("a\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			link := filepath.Join(e.Cwd(), "notes.md")
			if err := os.Symlink(target, link); err != nil {
				t.Skipf("symlinks are not available here: %v", err)
			}

			raw, err := json.Marshal(tc.args)
			if err != nil {
				t.Fatal(err)
			}
			var shown []string
			_, allowed := e.Approve(tc.tool, raw, func(tool, summary string) Decision {
				shown = append(shown, summary)
				return Decision{}
			})
			if allowed {
				t.Fatal("control failed: the write was not gated at all, so there is no prompt to judge")
			}
			if len(shown) != 1 {
				t.Fatalf("control failed: expected one prompt, got %d: %q", len(shown), shown)
			}
			if !strings.Contains(shown[0], target) {
				t.Fatalf("the prompt named %q; the file that would have been written is %q", shown[0], target)
			}
		})
	}
}

// The other half: an ordinary edit to an ordinary file must still read as that
// file and nothing else. A prompt that starts printing resolved absolute paths
// for every write would be worse than the one it replaced.
func TestAnOrdinaryWritePromptStillNamesThePathAsWritten(t *testing.T) {
	e := execIn(t, ModeManual)
	raw := json.RawMessage(`{"path":"src/main.go","content":"package main"}`)

	var shown []string
	_, allowed := e.Approve("write_file", raw, func(tool, summary string) Decision {
		shown = append(shown, summary)
		return Decision{}
	})
	if allowed || len(shown) != 1 {
		t.Fatalf("control failed: allowed=%v shown=%q", allowed, shown)
	}
	if shown[0] != "src/main.go" {
		t.Errorf("an ordinary in-tree write is described as %q, want %q", shown[0], "src/main.go")
	}
}
