package gitcmd

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A sweep rather than one key: the enumeration decides what to pin by matching
// a suffix, so the thing worth testing is every key anyone could think of that
// makes git run a program, against every command Tapioca actually issues. The
// unhardened run is printed so it is visible which of them this git build
// really executes — the rest prove only that the pin is harmless.
func TestNoConfigKeyExecutesForAnyCommandTapiocaRuns(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	dir := t.TempDir()
	markers := filepath.Join(dir, "markers")
	if err := os.MkdirAll(markers, 0o755); err != nil {
		t.Fatal(err)
	}
	hookFor := func(key string) string {
		p := filepath.Join(dir, "h_"+strings.ReplaceAll(key, ".", "_")+".sh")
		// cat, so a filter that is asked for content does not truncate the file.
		if err := os.WriteFile(p, []byte("#!/bin/sh\ntouch "+filepath.Join(markers, key)+"\ncat\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		return p
	}
	git := func(args ...string) {
		if out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput(); err != nil {
			t.Logf("setup %v: %v %s", args, err, out)
		}
	}
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(".gitattributes", "f.txt filter=ev diff=dv\n*.bin diff=dv\n")
	write("f.txt", "hi\n")
	write("a.bin", "\x00\x01binary\n")
	git("init", "-q", ".")
	git("config", "user.email", "t@e.com")
	git("config", "user.name", "t")
	for _, k := range []string{
		"core.fsmonitor", "core.hooksPath", "core.gitProxy", "core.alternateRefsCommand",
		"filter.ev.clean", "diff.dv.textconv", "diff.dv.command", "diff.external",
		"pager.status", "pager.diff", "pager.log", "interactive.diffFilter",
		"mergetool.x.cmd", "difftool.x.cmd", "trailer.sign.cmd", "gpg.program",
		"uploadpack.packObjectsHook", "credential.helper", "sequence.editor", "core.editor",
	} {
		git("config", k, hookFor(k))
	}
	git("config", "log.showSignature", "true")
	git("add", "-A")
	git("commit", "-q", "-m", "one")
	// Break the stat cache the way extracting an archive does, so status has to
	// run the clean filter to decide whether the file changed.
	for _, f := range []string{"f.txt", "a.bin"} {
		if err := os.Chtimes(filepath.Join(dir, f), time.Unix(1, 0), time.Unix(1, 0)); err != nil {
			t.Fatal(err)
		}
	}

	fired := func() []string {
		ents, _ := os.ReadDir(markers)
		var out []string
		for _, e := range ents {
			out = append(out, e.Name())
			_ = os.Remove(filepath.Join(markers, e.Name()))
		}
		return out
	}
	cmds := [][]string{
		{"status", "--porcelain", "-b"},
		{"diff", "--no-textconv", "--no-ext-diff"},
		{"diff", "--cached", "--no-textconv", "--no-ext-diff"},
		{"log", "-1", "--format=%h %s"},
		{"ls-files", "--cached", "--others"},
	}
	fired()
	for _, c := range cmds {
		_ = exec.Command("git", append([]string{"-C", dir}, c...)...).Run()
	}
	ran := fired()
	if len(ran) == 0 {
		t.Skip("this git build executes none of these here; the test cannot prove anything")
	}
	t.Logf("unhardened git executed: %v", ran)

	for _, c := range cmds {
		_ = In(dir, c...).Run()
	}
	if got := fired(); len(got) > 0 {
		t.Errorf("hardened git executed: %v", got)
	}
}
