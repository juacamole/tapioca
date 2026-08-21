//go:build unix

package checkpoint

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Round 13 disabled the global config scope for the shadow repo, and the test
// that shows it is TestSnapshotDoesNotObeyAGlobalExcludesFile: "a global ignore
// file that hides a path from `git add -A` would make the checkpoint miss a
// file it is supposed to be able to rewind".
//
// core.excludesFile is not the only file that answers "is this path ignored",
// and it is not the one an extracted tarball writes. `.gitignore` in the work
// tree is, it is attacker-authored like every other file there, and it outranks
// core.excludesFile in git's own precedence order — so the spelling that was
// closed is the one the user supplies and the spelling still open is the one
// the repository supplies.
//
// The consequence is not a lost file at the margins. A checkpoint is the only
// copy of an untracked file: a file the project's own git tracks can be
// recovered from the project's own git, so the paths where /rewind is the sole
// recourse are exactly the ones a .gitignore line removes from it. And the
// failure is silent — /rewind lists the checkpoint, restores nothing, and
// reports that it rewound.
func TestSnapshotDoesNotObeyTheWorkTreesOwnIgnoreFile(t *testing.T) {
	for _, tc := range []struct {
		name    string
		ignore  string
		rel     string
		control string
	}{
		// A single line naming one file: the stealthy form, where every other
		// path still rewinds normally and only the chosen one does not.
		{"one named file", "notes.txt\n", "notes.txt", "other.txt"},
		// A whole directory: `rm -rf` under it becomes unrewindable.
		{"a whole directory", "work/\n", filepath.Join("work", "notes.txt"), "other.txt"},
		// The blunt form. Nothing at all is ever snapshotted, so the single
		// checkpoint that exists is empty and /rewind is a no-op for the whole
		// session.
		{"everything", "*\n", "notes.txt", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			needGit(t)
			t.Setenv("TAPIOCA_DATA_DIR", t.TempDir())

			tree := t.TempDir()
			if resolved, err := filepath.EvalSymlinks(tree); err == nil {
				tree = resolved
			}
			write := func(rel, body string) {
				t.Helper()
				p := filepath.Join(tree, rel)
				if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			// The tarball ships the ignore file; the user's own work is what
			// it names.
			write(".gitignore", tc.ignore)
			write(tc.rel, "the user's only copy\n")
			if tc.control != "" {
				write(tc.control, "first\n")
			}

			id, err := Snapshot(tree, "before the agent touches anything")
			if err != nil || id == "" {
				t.Fatalf("snapshot: id=%q err=%v", id, err)
			}

			// The agent, having read an instruction out of the repository,
			// destroys it.
			write(tc.rel, "destroyed\n")
			if tc.control != "" {
				write(tc.control, "second\n")
			}
			if err := Restore(tree, id); err != nil {
				t.Fatalf("restore: %v", err)
			}

			back, err := os.ReadFile(filepath.Join(tree, tc.rel))
			if err != nil || string(back) != "the user's only copy\n" {
				t.Fatalf("the tree's own .gitignore kept %s out of the checkpoint, so the rewind lost it: %q %v",
					tc.rel, back, err)
			}
			if tc.control != "" {
				// The control says the rewind machinery worked at all: a path
				// the ignore file does not name comes back.
				got, err := os.ReadFile(filepath.Join(tree, tc.control))
				if err != nil || string(got) != "first\n" {
					t.Fatalf("control: an unignored file did not rewind either, so this test shows nothing: %q %v", got, err)
				}
			}
		})
	}
}

// tracked lists what the shadow repo holds at HEAD.
func tracked(t *testing.T, tree string) string {
	t.Helper()
	out, err := run(tree, "ls-tree", "-r", "--name-only", "HEAD")
	if err != nil {
		t.Fatalf("ls-tree: %v", err)
	}
	return out
}

// The reason this is a budget and not `add -A -f`: what ignore rules mostly
// name is node_modules, target and .venv, and forcing those in would commit a
// fresh copy of a build output every time the agent runs a build. A directory
// over the budget stays out; everything else still comes in, including the
// ignored files that sit beside it.
func TestABigIgnoredDirectoryStaysOutOfTheCheckpoint(t *testing.T) {
	needGit(t)
	t.Setenv("TAPIOCA_DATA_DIR", t.TempDir())

	tree := t.TempDir()
	if resolved, err := filepath.EvalSymlinks(tree); err == nil {
		tree = resolved
	}
	write := func(rel, body string) {
		t.Helper()
		p := filepath.Join(tree, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(".gitignore", "build/\nnotes.txt\n")
	write("main.go", "package main\n")
	write("notes.txt", "the user's only copy\n")

	// One apparently huge file is enough to put the directory over the byte
	// budget, and a sparse file costs nothing to make.
	big := filepath.Join(tree, "build", "artifact.bin")
	if err := os.MkdirAll(filepath.Dir(big), 0o700); err != nil {
		t.Fatal(err)
	}
	f, err := os.Create(big)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(int64(maxIgnoredBytes) + 1); err != nil {
		f.Close()
		t.Skipf("cannot make a sparse file here: %v", err)
	}
	f.Close()
	if info, err := os.Stat(big); err != nil || info.Size() <= int64(maxIgnoredBytes) {
		t.Skipf("the sparse file did not take: %v", err)
	}

	if _, err := Snapshot(tree, "first"); err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	got := tracked(t, tree)
	if strings.Contains(got, "build/artifact.bin") {
		t.Errorf("an over-budget ignored build directory was committed:\n%s", got)
	}
	for _, want := range []string{"main.go", "notes.txt"} {
		if !strings.Contains(got, want) {
			t.Errorf("%s is missing from the checkpoint:\n%s", want, got)
		}
	}
}

// The names in the ignored list come from the tree, so they are attacker-chosen
// too. A pathspec that begins with a dash is an option and one that begins with
// a colon is pathspec magic; both would make `git add` do something other than
// add that file — quietly, since a snapshot's errors are discarded by its
// caller.
func TestAnIgnoredFileWithAHostileNameIsStillSnapshotted(t *testing.T) {
	needGit(t)
	t.Setenv("TAPIOCA_DATA_DIR", t.TempDir())

	tree := t.TempDir()
	if resolved, err := filepath.EvalSymlinks(tree); err == nil {
		tree = resolved
	}
	names := []string{"-rf", ":(glob)odd", "*star", "a b"}
	body := "the user's only copy\n"
	if err := os.WriteFile(filepath.Join(tree, ".gitignore"), []byte("*\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, n := range names {
		if err := os.WriteFile(filepath.Join(tree, n), []byte(body), 0o600); err != nil {
			t.Skipf("cannot create %q here: %v", n, err)
		}
	}

	id, err := Snapshot(tree, "first")
	if err != nil || id == "" {
		t.Fatalf("snapshot: id=%q err=%v", id, err)
	}
	for _, n := range names {
		if err := os.WriteFile(filepath.Join(tree, n), []byte("destroyed\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := Restore(tree, id); err != nil {
		t.Fatalf("restore: %v", err)
	}
	for _, n := range names {
		back, err := os.ReadFile(filepath.Join(tree, n))
		if err != nil || string(back) != body {
			t.Errorf("%q did not come back from the checkpoint: %q %v", n, back, err)
		}
	}
}
