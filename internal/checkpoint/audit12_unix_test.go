//go:build unix

package checkpoint

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// The shadow repo pins the keys that name a program, and the comment in run()
// explains why the worktree's own config is not a source: git is pointed at a
// git dir under the data directory, so .git/config in the tree is never read.
//
// That is true of the *files*. Configuration also reaches git through the
// environment, and the environment is reachable from an extracted tree through
// an .envrc — the same door the GIT_CONFIG_PARAMETERS finding came through.
// GIT_CONFIG_COUNT / GIT_CONFIG_KEY_<n> / GIT_CONFIG_VALUE_<n> define any key
// at all, and GIT_CONFIG_GLOBAL names a whole config file, which may sit inside
// the worktree.
//
// A pin cannot answer either of those, because the dangerous key here is
// filter.<name>.clean and the repository invents <name> and selects it from its
// own .gitattributes. gitcmd survives the same trick only because it enumerates
// the configuration with `git config --list` before pinning; this caller does
// not enumerate anything.
//
// What that buys: a checkpoint is taken before every mutating tool call, and
// `git add -A` runs the clean filter over the worktree. So one write — in auto
// mode, one nobody is asked about; otherwise one whose summary says "edit
// README.md" — runs whatever the repository chose.
//
// The build tag is because the payload is a shell command line, which is what
// git runs a clean filter as.
func TestSnapshotIgnoresConfigFromTheEnvironment(t *testing.T) {
	needGit(t)

	tree := hostileTree(t, "p")
	marker := filepath.Join(tree, "executed")
	clean := "sh -c 'printf x > " + marker + "'; cat"

	env := []string{
		"GIT_CONFIG_COUNT=1",
		"GIT_CONFIG_KEY_0=filter.p.clean",
		"GIT_CONFIG_VALUE_0=" + clean,
	}
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
		t.Fatal("the checkpoint ran a filter driver defined in the environment: a tree that can export one variable executes code on the next approved write")
	}
}

// GIT_CONFIG_GLOBAL is the same hole by a different variable, and a worse one
// to leave: it names a file, and the file it names can be one the repository
// ships.
func TestSnapshotIgnoresAGlobalConfigNamedByTheEnvironment(t *testing.T) {
	needGit(t)

	tree := hostileTree(t, "q")
	marker := filepath.Join(tree, "executed")
	conf := filepath.Join(tree, "shipped.gitconfig")
	body := "[filter \"q\"]\n\tclean = sh -c 'printf x > " + marker + "'; cat\n"
	if err := os.WriteFile(conf, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	env := []string{"GIT_CONFIG_GLOBAL=" + conf}
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
		t.Fatal("the checkpoint read a global config the tree pointed it at, and ran the filter in it")
	}
}

// An ordinary snapshot, with the same worktree layout minus the hostile
// environment, still records the tree and can still be rewound. Losing the
// checkpoint would be a worse outcome than the bug.
func TestSnapshotAndRestoreStillWork(t *testing.T) {
	needGit(t)

	tree := t.TempDir()
	if resolved, err := filepath.EvalSymlinks(tree); err == nil {
		tree = resolved
	}
	t.Setenv("TAPIOCA_DATA_DIR", t.TempDir())

	file := filepath.Join(tree, "keep.txt")
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
		t.Fatalf("rewind did not restore the file: %q %v", back, err)
	}
}

func needGit(t *testing.T) {
	t.Helper()
	if !Available() {
		t.Skip("git is not installed here")
	}
}

// hostileTree is a worktree whose .gitattributes selects a filter driver by
// name. The driver itself is not defined here — that is the environment's job.
func hostileTree(t *testing.T, filter string) string {
	t.Helper()
	dir := t.TempDir()
	if resolved, err := filepath.EvalSymlinks(dir); err == nil {
		dir = resolved
	}
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("f.txt", "content\n")
	write(".gitattributes", "f.txt filter="+filter+"\n")
	return dir
}

// requireFilterFires is the control: it drives a plain `git add -A` with the
// same environment, in a throwaway git dir, and skips unless the marker
// appears. Without it a git that ignored the variable — an old build, a
// distribution that disables filters — would make the assertions above pass
// while proving nothing.
func requireFilterFires(t *testing.T, tree, marker string, env []string) {
	t.Helper()
	base := append(os.Environ(), env...)
	base = append(base, "GIT_DIR="+t.TempDir(), "GIT_WORK_TREE="+tree)
	for _, args := range [][]string{{"init", "-q"}, {"add", "-A"}} {
		cmd := exec.Command("git", args...)
		cmd.Env = base
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Skipf("the control run could not be set up (git %v: %v: %s)", args, err, out)
		}
	}
	if _, err := os.Stat(marker); err != nil {
		t.Skip("this git does not run a clean filter defined this way; there is nothing here to block")
	}
	if err := os.Remove(marker); err != nil {
		t.Fatal(err)
	}
}

func cut(kv string) (string, string, bool) {
	for i := 0; i < len(kv); i++ {
		if kv[i] == '=' {
			return kv[:i], kv[i+1:], true
		}
	}
	return kv, "", false
}
