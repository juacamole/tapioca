package gitcmd

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// A repository's .git/config names programs git will run. core.fsmonitor is
// the sharpest: `git status` executes it, and the git panel polls status every
// few seconds, so opening a directory — in any mode, including plan, before
// the user has typed anything — ran whatever that directory chose.
func TestRepoConfigCannotExecuteAProgram(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	dir := t.TempDir()
	marker := filepath.Join(dir, "EXECUTED")
	hook := filepath.Join(dir, "hook.sh")
	if err := os.WriteFile(hook, []byte("#!/bin/sh\ntouch "+marker+"\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"init", "-q", "."},
		{"config", "core.fsmonitor", hook},
	} {
		if out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("setup %v: %v %s", args, err, out)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	// The unhardened form is what used to run, and it is the control: if this
	// stops executing the hook, the test is no longer proving anything.
	_ = exec.Command("git", "-C", dir, "status", "--porcelain", "-b").Run()
	if _, err := os.Stat(marker); err != nil {
		t.Skip("this git build does not run core.fsmonitor; the test cannot prove anything")
	}
	if err := os.Remove(marker); err != nil {
		t.Fatal(err)
	}

	_ = In(dir, "status", "--porcelain", "-b").Run()
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("the hardened command still executed the repository's fsmonitor program")
	}
}

// The hardening must not stop git from answering.
func TestHardenedGitStillWorks(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	dir := t.TempDir()
	for _, args := range [][]string{
		{"init", "-q", "."},
		{"config", "user.email", "t@example.com"},
		{"config", "user.name", "t"},
	} {
		if out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("setup: %v %s", err, out)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := In(dir, "status", "--porcelain", "-b").Output()
	if err != nil {
		t.Fatalf("hardened status failed: %v", err)
	}
	if len(out) == 0 {
		t.Fatal("hardened status returned nothing")
	}
}

// runs reports whether the given git invocation executed the repo's program.
func runs(t *testing.T, dir, marker string, run func()) bool {
	t.Helper()
	os.Remove(marker)
	run()
	_, err := os.Stat(marker)
	return err == nil
}

// A fixed -c list cannot pre-empt filter.<name>.clean, because the repository
// invents the name and selects it with its own .gitattributes. git runs the
// clean filter during `status` whenever stat data does not match the index,
// which is the normal state after extracting a tarball or copying a tree.
func TestCleanFilterCannotExecute(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	dir := t.TempDir()
	marker := filepath.Join(dir, "FILTER_RAN")
	hook := filepath.Join(dir, "pwn.sh")
	if err := os.WriteFile(hook, []byte("#!/bin/sh\ntouch "+marker+"\ncat\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(".gitattributes", "tracked.txt filter=evil\n")
	write("tracked.txt", "hello\n")
	for _, args := range [][]string{
		{"init", "-q", "."},
		{"config", "filter.evil.clean", hook},
		{"config", "user.email", "t@example.com"},
		{"config", "user.name", "t"},
		{"add", "-A"},
	} {
		if out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("setup %v: %v %s", args, err, out)
		}
	}
	// Break the stat cache the way extracting an archive does.
	old := filepath.Join(dir, "tracked.txt")
	if err := os.Chtimes(old, time.Unix(1, 0), time.Unix(1, 0)); err != nil {
		t.Fatal(err)
	}

	if !runs(t, dir, marker, func() {
		_ = exec.Command("git", "-C", dir, "status", "--porcelain", "-b").Run()
	}) {
		t.Skip("this git does not run the clean filter here; the test cannot prove anything")
	}
	if runs(t, dir, marker, func() { _ = In(dir, "status", "--porcelain", "-b").Run() }) {
		t.Error("hardened status ran the repository's clean filter")
	}
	if runs(t, dir, marker, func() {
		_ = In(dir, "diff", "--no-textconv", "--no-ext-diff").Run()
	}) {
		t.Error("hardened diff ran the repository's clean filter")
	}
}

// A filter driver name may legally contain '=': [filter "a=b"] selected by
// `filter=a=b` in .gitattributes. Pinning it with `git -c filter.a=b.clean=`
// fails, because -c splits on the first '=' and sets key "filter.a" instead,
// leaving the real clean command live during `git diff`. The pins therefore go
// through GIT_CONFIG_* env, which keeps key and value apart.
func TestFilterNameWithEqualsCannotExecute(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	dir := t.TempDir()
	marker := filepath.Join(dir, "RAN")
	hook := filepath.Join(dir, "pwn.sh")
	if err := os.WriteFile(hook, []byte("#!/bin/sh\ntouch "+marker+"\ncat\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, f := range [][2]string{{".gitattributes", "* filter=a=b\n"}, {"f.txt", "hi\n"}} {
		if err := os.WriteFile(filepath.Join(dir, f[0]), []byte(f[1]), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for _, args := range [][]string{
		{"init", "-q", "."}, {"config", "user.email", "t@e.com"}, {"config", "user.name", "t"},
		{"config", "filter.a=b.clean", hook}, {"config", "filter.a=b.required", "true"},
		{"add", "-A"},
	} {
		if out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("setup %v: %v %s", args, err, out)
		}
	}
	if err := os.Chtimes(filepath.Join(dir, "f.txt"), time.Unix(1, 0), time.Unix(1, 0)); err != nil {
		t.Fatal(err)
	}
	// git runs clean filters during diff (the /diff slash command), so this is
	// the control the bypass rode in on.
	if !runs(t, dir, marker, func() {
		_ = exec.Command("git", "-C", dir, "diff").Run()
	}) {
		t.Skip("this git does not run the clean filter here; the test cannot prove anything")
	}
	if runs(t, dir, marker, func() {
		_ = In(dir, "diff", "--no-textconv", "--no-ext-diff").Run()
	}) {
		t.Error("hardened diff ran a clean filter whose driver name contains '='")
	}
	if runs(t, dir, marker, func() { _ = In(dir, "status", "--porcelain", "-b").Run() }) {
		t.Error("hardened status ran a clean filter whose driver name contains '='")
	}
}

// log.showSignature plus gpg.program is a second, independent path.
func TestSignatureVerificationCannotExecute(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	dir := t.TempDir()
	marker := filepath.Join(dir, "GPG_RAN")
	hook := filepath.Join(dir, "pwn.sh")
	if err := os.WriteFile(hook, []byte("#!/bin/sh\ntouch "+marker+"\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"init", "-q", "."},
		{"config", "user.email", "t@example.com"},
		{"config", "user.name", "t"},
		{"config", "log.showSignature", "true"},
		{"config", "gpg.program", hook},
		{"commit", "-q", "--allow-empty", "-m", "first"},
	} {
		if out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("setup %v: %v %s", args, err, out)
		}
	}
	if runs(t, dir, marker, func() {
		_ = In(dir, "log", "-1", "--format=%h %s").Run()
	}) {
		t.Error("hardened log ran the repository's gpg program")
	}
}

// setupFilterRepo builds a repo whose clean filter is defined wherever the
// caller puts it, and returns the marker path the filter touches.
func setupFilterRepo(t *testing.T, defineIn func(dir, hook string)) (dir, marker string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	dir = t.TempDir()
	marker = filepath.Join(dir, "RAN")
	hook := filepath.Join(dir, "pwn.sh")
	if err := os.WriteFile(hook, []byte("#!/bin/sh\ntouch "+marker+"\ncat\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, f := range [][2]string{{".gitattributes", "* filter=ev\n"}, {"f.txt", "hi\n"}} {
		if err := os.WriteFile(filepath.Join(dir, f[0]), []byte(f[1]), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for _, args := range [][]string{
		{"init", "-q", "."}, {"config", "user.email", "t@e.com"}, {"config", "user.name", "t"},
		{"add", "-A"},
	} {
		if out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("setup %v: %v %s", args, err, out)
		}
	}
	defineIn(dir, hook)
	if err := os.Chtimes(filepath.Join(dir, "f.txt"), time.Unix(1, 0), time.Unix(1, 0)); err != nil {
		t.Fatal(err)
	}
	return dir, marker
}

// git config --local reads one file: it does not expand include.path, so a
// filter defined in the included file was invisible to the enumeration.
func TestIncludedConfigCannotExecute(t *testing.T) {
	dir, marker := setupFilterRepo(t, func(dir, hook string) {
		extra := filepath.Join(dir, ".git", "extra.cfg")
		if err := os.WriteFile(extra, []byte("[filter \"ev\"]\n\tclean = "+hook+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if out, err := exec.Command("git", "-C", dir, "config", "include.path", "extra.cfg").CombinedOutput(); err != nil {
			t.Fatalf("%v %s", err, out)
		}
	})
	if !runs(t, dir, marker, func() {
		_ = exec.Command("git", "-C", dir, "status", "--porcelain", "-b").Run()
	}) {
		t.Skip("this git does not run the filter here")
	}
	if runs(t, dir, marker, func() { _ = In(dir, "status", "--porcelain", "-b").Run() }) {
		t.Error("a filter defined through include.path still executed")
	}
}

// The pins are read once and cached, and a cache is only as good as its key.
// The keys git obeys do not all live in .git/config: an include.path names
// another file, and the repository chooses where that file is — including
// inside the worktree, where an ordinary edit reaches it without ever looking
// like a change to git's configuration. Writing a filter driver into it leaves
// .git/config's own size and mtime untouched, so a cache keyed on those still
// answers with the pins from before, and the new filter.<name>.clean is live
// during the next status poll.
func TestPinsNoticeAKeyAddedToAnIncludedFileAfterTheFirstRead(t *testing.T) {
	dir, marker := setupFilterRepo(t, func(dir, hook string) {
		// The included file starts benign. .git/config is written here and
		// never again, which is the whole point.
		if err := os.WriteFile(filepath.Join(dir, "lint.cfg"), []byte("[core]\n\tautocrlf = false\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if out, err := exec.Command("git", "-C", dir, "config", "include.path", "../lint.cfg").CombinedOutput(); err != nil {
			t.Fatalf("%v %s", err, out)
		}
	})
	stale := filepath.Join(dir, "f.txt")
	dirty := func() {
		if err := os.Chtimes(stale, time.Unix(1, 0), time.Unix(1, 0)); err != nil {
			t.Fatal(err)
		}
	}
	// Opening the directory: the git panel polls status, and the pins for this
	// repository are read and cached while nothing dangerous is configured.
	dirty()
	_ = In(dir, "status", "--porcelain", "-b").Run()

	// An in-tree edit — auto mode approves one without asking — turns the
	// included file into a filter driver.
	hook := filepath.Join(dir, "pwn.sh")
	if err := os.WriteFile(filepath.Join(dir, "lint.cfg"), []byte("[filter \"ev\"]\n\tclean = "+hook+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	dirty()
	if !runs(t, dir, marker, func() {
		_ = exec.Command("git", "-C", dir, "status", "--porcelain", "-b").Run()
	}) {
		t.Skip("this git does not run the filter here")
	}
	dirty()
	if runs(t, dir, marker, func() { _ = In(dir, "status", "--porcelain", "-b").Run() }) {
		t.Error("a filter written into an included file after the pins were cached still executed")
	}
}

// Noticing a change must not degenerate into never caching: the pins are read
// on a five-second timer, so the check has to be able to say "unchanged". A
// working directory below the repository root is included, because git reports
// .git/config by a path relative to the top of the worktree and an unresolved
// relative path is refused rather than guessed at.
func TestPinsStayCachedWhileNothingChanges(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	root := t.TempDir()
	sub := filepath.Join(root, "sub")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"init", "-q", "."}, {"config", "user.email", "t@e.com"}} {
		if out, err := exec.Command("git", append([]string{"-C", root}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("setup %v: %v %s", args, err, out)
		}
	}
	for _, dir := range []string{root, sub} {
		repoPins(dir)
		pinMu.Lock()
		c := pinsByRepo[dir]
		pinMu.Unlock()
		if len(c.files) == 0 {
			t.Fatalf("%s: no config origins recorded, so every git command re-reads the pins", dir)
		}
		for _, f := range c.files {
			if !filepath.IsAbs(f.path) {
				t.Errorf("%s: origin %q is not absolute", dir, f.path)
			}
		}
		if !current(c.files) {
			t.Errorf("%s: the pins were reported stale with nothing changed", dir)
		}
	}
	// A key added to the repository's own config is still noticed.
	if out, err := exec.Command("git", "-C", root, "config", "core.fsmonitor", "/bin/true").CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	pinMu.Lock()
	c := pinsByRepo[root]
	pinMu.Unlock()
	if current(c.files) {
		t.Error("an edit to .git/config left the cache reporting itself current")
	}
}

// The suffix rule was written for the sectioned spelling (diff.x.textconv) and
// git also glues the same word onto a component (core.sshCommand). Every glued
// name anyone listed is in the exact list, which is what makes the one nobody
// listed — core.alternateRefsCommand — the interesting case: it is a program
// git runs, and ".command" does not end "…refscommand".
//
// The second half matters as much: pinning a key to empty changes what git
// does, so an ordinary key must not be caught by widening the match.
func TestKeysThatNameAProgramAreRecognisedInBothSpellings(t *testing.T) {
	for _, k := range []string{
		"core.alternateRefsCommand", "core.sshCommand", "core.askPass",
		"uploadpack.packObjectsHook", "diff.myconv.textconv", "merge.mine.driver",
		"mergetool.x.cmd", "trailer.sign.cmd", "gpg.ssh.program",
		"credential.https://example.com.helper", "remote.origin.uploadpack",
	} {
		if !namesProgram(k) {
			t.Errorf("%s runs a program and is not pinned", k)
		}
	}
	for _, k := range []string{
		"user.name", "user.email", "core.autocrlf", "core.filemode", "core.bare",
		"init.defaultBranch", "pull.rebase", "color.ui", "alias.st", "alias.lg",
		"branch.main.remote", "remote.origin.url", "diff.algorithm", "commit.gpgsign",
	} {
		if namesProgram(k) {
			t.Errorf("%s is an ordinary key and pinning it changes what git does", k)
		}
	}
}

// --show-origin is what tells the cache which files to watch, and it is also a
// flag that a git old enough not to have it rejects — which would turn the
// whole enumeration into an error and leave the repository with no pins at
// all. The flag is therefore optional: without it the pins are still read, and
// nothing is cached because nothing is known about where the config came from.
func TestPinsSurviveAGitWithoutShowOrigin(t *testing.T) {
	real, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git not available")
	}
	shim := t.TempDir()
	script := "#!/bin/sh\nfor a in \"$@\"; do\n  [ \"$a\" = \"--show-origin\" ] && exit 129\ndone\nexec " + real + " \"$@\"\n"
	if err := os.WriteFile(filepath.Join(shim, "git"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", shim+string(os.PathListSeparator)+os.Getenv("PATH"))
	if out, err := exec.Command("git", "config", "--list", "--show-origin").CombinedOutput(); err == nil {
		t.Skipf("the shim did not take effect: %s", out)
	}

	dir, marker := setupFilterRepo(t, func(dir, hook string) {
		if out, err := exec.Command("git", "-C", dir, "config", "filter.ev.clean", hook).CombinedOutput(); err != nil {
			t.Fatalf("%v %s", err, out)
		}
	})
	if !runs(t, dir, marker, func() {
		_ = exec.Command("git", "-C", dir, "status", "--porcelain", "-b").Run()
	}) {
		t.Skip("this git does not run the filter here")
	}
	if runs(t, dir, marker, func() { _ = In(dir, "status", "--porcelain", "-b").Run() }) {
		t.Error("a git that rejects --show-origin was left with no repository pins at all")
	}
	pinMu.Lock()
	c := pinsByRepo[dir]
	pinMu.Unlock()
	if len(c.pins) == 0 {
		t.Error("no pins were read without --show-origin")
	}
	if len(c.files) != 0 {
		t.Error("files were recorded although no origin was reported")
	}
}

// Worktree scope is a second file --local never reads.
func TestWorktreeConfigCannotExecute(t *testing.T) {
	dir, marker := setupFilterRepo(t, func(dir, hook string) {
		for _, args := range [][]string{
			{"config", "extensions.worktreeConfig", "true"},
			{"config", "--worktree", "filter.ev.clean", hook},
		} {
			if out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput(); err != nil {
				t.Skipf("worktree config unsupported: %v %s", err, out)
			}
		}
	})
	if !runs(t, dir, marker, func() {
		_ = exec.Command("git", "-C", dir, "status", "--porcelain", "-b").Run()
	}) {
		t.Skip("this git does not run the filter here")
	}
	if runs(t, dir, marker, func() { _ = In(dir, "status", "--porcelain", "-b").Run() }) {
		t.Error("a filter defined in worktree scope still executed")
	}
}
