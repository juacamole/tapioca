package checkpoint

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"tapioca/internal/secretenv"
)

// The shadow repo lives at a path derived from the work tree, so anything that
// gets one write outside the working directory can leave a pre-commit hook
// there and be run again by every later snapshot, in this session and the next.
// The control is the same commit with the pins taken away: if that does not run
// the hook, this machine cannot show the difference and the test proves nothing.
func TestPlantedCheckpointHookDoesNotRun(t *testing.T) {
	if !Available() {
		t.Skip("no git")
	}
	data := t.TempDir()
	t.Setenv("TAPIOCA_DATA_DIR", data)
	tree := t.TempDir()
	if err := os.WriteFile(filepath.Join(tree, "a.txt"), []byte("one"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Snapshot(tree, "first"); err != nil {
		t.Fatal(err)
	}

	marker := filepath.Join(data, "pwned")
	hooks := filepath.Join(gitDir(tree), "hooks")
	if err := os.MkdirAll(hooks, 0o755); err != nil {
		t.Fatal(err)
	}
	hook := "#!/bin/sh\ntouch " + marker + "\n"
	if err := os.WriteFile(filepath.Join(hooks, "pre-commit"), []byte(hook), 0o755); err != nil {
		t.Fatal(err)
	}

	// Control: unpinned, the planted hook runs.
	if err := os.WriteFile(filepath.Join(tree, "a.txt"), []byte("two"), 0o644); err != nil {
		t.Fatal(err)
	}
	unpinned := func(args ...string) {
		t.Helper()
		c := exec.Command("git", args...)
		c.Env = append(secretenv.Scrubbed(),
			"GIT_DIR="+gitDir(tree), "GIT_WORK_TREE="+tree,
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		_ = c.Run()
	}
	unpinned("add", "-A")
	unpinned("commit", "-q", "-m", "control")
	if _, err := os.Stat(marker); err != nil {
		t.Skipf("control failed: git did not run the planted hook here (%v)", err)
	}
	os.Remove(marker)

	if err := os.WriteFile(filepath.Join(tree, "a.txt"), []byte("three"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Snapshot(tree, "second"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Error("a hook planted in the checkpoint repo ran during a snapshot")
	}
}
