package gitcmd

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// The other end of the checkpoint finding, asked here because the answer is
// different and the difference is the whole design.
//
// checkpoint pins a fixed list of keys and has no way to see a
// filter.<name>.clean the tree invented, so it had to shut the global scope off
// once $HOME turned out to select it. This package pins nothing blindly: it
// runs `git config --list`, which reports every scope — including the global
// file $HOME or $XDG_CONFIG_HOME names — and pins whatever comes back. A tree
// that exports HOME=$PWD/.home and commits .home/.gitconfig therefore gets its
// filter enumerated and neutralised rather than run.
//
// That is a property of reading every scope, and it would be lost the day
// listConfig grows a `--local` for speed. The test is here so that day fails.
func TestAGlobalConfigSelectedByHomeIsEnumeratedAndPinned(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	for _, tc := range []struct{ name, rel string }{
		{"HOME", ".gitconfig"},
		{"XDG_CONFIG_HOME", filepath.Join("git", "config")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			marker := filepath.Join(dir, "FILTER_RAN")
			hook := filepath.Join(dir, "pwn.sh")
			if err := os.WriteFile(hook, []byte("#!/bin/sh\ntouch "+marker+"\ncat\n"), 0o755); err != nil {
				t.Fatal(err)
			}
			home := filepath.Join(dir, ".home")
			conf := filepath.Join(home, tc.rel)
			if err := os.MkdirAll(filepath.Dir(conf), 0o700); err != nil {
				t.Fatal(err)
			}
			body := "[user]\n\tname = t\n\temail = t@example.com\n" +
				"[filter \"evil\"]\n\tclean = " + hook + "\n"
			if err := os.WriteFile(conf, []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			// Both, every time: git reads $XDG_CONFIG_HOME/git/config and
			// $HOME/.gitconfig both, so leaving one pointing at the real home
			// would let the machine's own global config answer as well.
			t.Setenv("HOME", home)
			t.Setenv("XDG_CONFIG_HOME", home)

			write := func(name, body string) {
				if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			write(".gitattributes", "tracked.txt filter=evil\n")
			write("tracked.txt", "hello\n")
			for _, args := range [][]string{{"init", "-q", "."}, {"add", "-A"}} {
				if out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput(); err != nil {
					t.Fatalf("setup %v: %v %s", args, err, out)
				}
			}
			// Break the stat cache the way extracting an archive does, so
			// status has to re-read the file and run the filter over it.
			f := filepath.Join(dir, "tracked.txt")
			if err := os.Chtimes(f, time.Unix(1, 0), time.Unix(1, 0)); err != nil {
				t.Fatal(err)
			}

			if !runs(t, dir, marker, func() {
				_ = exec.Command("git", "-C", dir, "status", "--porcelain", "-b").Run()
			}) {
				t.Skip("this git does not run the clean filter here; the test cannot prove anything")
			}
			// status is the one polled every few seconds for the git panel, in
			// every permission mode including plan.
			if runs(t, dir, marker, func() { _ = In(dir, "status", "--porcelain", "-b").Run() }) {
				t.Error("hardened status ran a clean filter defined in a global config the environment pointed at")
			}
			if runs(t, dir, marker, func() {
				_ = In(dir, "diff", "--no-textconv", "--no-ext-diff").Run()
			}) {
				t.Error("hardened diff ran a clean filter defined in a global config the environment pointed at")
			}
		})
	}
}
