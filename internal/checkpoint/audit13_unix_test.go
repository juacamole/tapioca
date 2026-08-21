//go:build unix

package checkpoint

import (
	"os"
	"path/filepath"
	"testing"
)

// GIT_CONFIG_GLOBAL was blocked because it names the file git reads the global
// scope from. It is not the only variable that does: $HOME is where git looks
// for .gitconfig when nothing names it, and $XDG_CONFIG_HOME is where it looks
// for git/config. Both are ordinary environment variables, and the environment
// is reachable from an extracted tree through an .envrc — the same door every
// other finding in this file came through.
//
// So `export HOME=$PWD/.home`, a committed .home/.gitconfig defining
// filter.p.clean, and `* filter=p` in .gitattributes reach exactly the value
// the explicit variable was refused for, by naming the directory instead of the
// file. The comment in run() says the worktree's config is never a source
// because git is pointed at a git dir under the data directory; that is true of
// the repository scope and was never true of the global one, which is chosen by
// the environment.
//
// A checkpoint is taken before every mutating tool call and `git add -A` runs
// the clean filter over the worktree, so one approved write — in auto mode, one
// nobody is asked about — executes whatever the tree chose.
func TestSnapshotIgnoresAGlobalConfigNamedByHome(t *testing.T) {
	for _, tc := range []struct {
		name    string
		envName string
		rel     string
	}{
		{"HOME", "HOME", ".gitconfig"},
		{"XDG_CONFIG_HOME", "XDG_CONFIG_HOME", filepath.Join("git", "config")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			needGit(t)

			tree := hostileTree(t, "p")
			marker := filepath.Join(tree, "executed")
			// The fake home sits inside the worktree, which is what an .envrc
			// pointing at $PWD gets: the tree ships both the variable and the
			// file it selects.
			fakeHome := filepath.Join(tree, ".home")
			conf := filepath.Join(fakeHome, tc.rel)
			if err := os.MkdirAll(filepath.Dir(conf), 0o700); err != nil {
				t.Fatal(err)
			}
			body := "[filter \"p\"]\n\tclean = sh -c 'printf x > " + marker + "'; cat\n"
			if err := os.WriteFile(conf, []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}

			// Both variables are set for the control and for the run: git
			// consults XDG_CONFIG_HOME and $HOME/.gitconfig both, and pinning
			// only the one under test would let the machine's real global
			// config answer instead.
			env := []string{"HOME=" + fakeHome, "XDG_CONFIG_HOME=" + fakeHome}
			requireFilterFires(t, tree, marker, env)

			for _, kv := range env {
				k, v, _ := cut(kv)
				t.Setenv(k, v)
			}
			t.Setenv("TAPIOCA_DATA_DIR", t.TempDir())

			if _, err := Snapshot(tree, "an ordinary edit"); err != nil {
				t.Fatalf("snapshot failed: %v", err)
			}
			if _, err := os.Stat(marker); err == nil {
				t.Fatalf("the checkpoint read a global config selected by $%s and ran the filter in it", tc.envName)
			}
		})
	}
}

// The user's own global config must stop reaching the shadow repo, and that has
// to be visible as more than the absence of an attack: a snapshot taken with a
// global config in place must not pick anything up from it. core.excludesFile
// is the observable one — a global ignore file that hides a path from `git add
// -A` would make the checkpoint miss a file it is supposed to be able to rewind.
func TestSnapshotDoesNotObeyAGlobalExcludesFile(t *testing.T) {
	needGit(t)

	tree := t.TempDir()
	if resolved, err := filepath.EvalSymlinks(tree); err == nil {
		tree = resolved
	}
	home := t.TempDir()
	ignore := filepath.Join(home, "ignore")
	if err := os.WriteFile(ignore, []byte("secret.txt\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	conf := "[core]\n\texcludesFile = " + ignore + "\n"
	if err := os.WriteFile(filepath.Join(home, ".gitconfig"), []byte(conf), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", home)
	t.Setenv("TAPIOCA_DATA_DIR", t.TempDir())

	file := filepath.Join(tree, "secret.txt")
	if err := os.WriteFile(file, []byte("first\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	id, err := Snapshot(tree, "first")
	if err != nil || id == "" {
		t.Fatalf("snapshot: id=%q err=%v", id, err)
	}
	if err := os.WriteFile(file, []byte("second\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Restore(tree, id); err != nil {
		t.Fatalf("restore: %v", err)
	}
	back, err := os.ReadFile(file)
	if err != nil || string(back) != "first\n" {
		t.Fatalf("a globally ignored file was not snapshotted, so the rewind lost it: %q %v", back, err)
	}
}
