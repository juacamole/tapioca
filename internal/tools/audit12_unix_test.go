//go:build unix

package tools

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// ripgrep reads command-line arguments from the file named by
// $RIPGREP_CONFIG_PATH, and `--pre=COMMAND` is one of the arguments it accepts
// there: rg then runs COMMAND on every file it searches. The environment is
// reachable from the tree — an .envrc in an extracted tarball, direnv, exactly
// the channel the git config finding used — so a repository could hand grep a
// command to run.
//
// grep is the worst tool for that to be true of. It is non-mutating, so it
// never reaches the permission gate and the user is never asked; it is one of
// the tools plan mode allows, so the tree executes before anything at all has
// been approved. And it only happens on a machine that has ripgrep installed,
// which is to say nearly every developer's and not CI's.
//
// The build tag is because the payload has to be a program: a shell script is
// the shortest one that is definitely present here, and there is no /bin/sh on
// Windows.
func TestGrepDoesNotRunRipgrepConfigPreprocessor(t *testing.T) {
	needRipgrep(t)

	e := grepTree(t, map[string]string{"src/hay.txt": "a needle in here\n"})

	// Somewhere rg will not be searching, so the payload and its evidence are
	// not themselves results.
	aside := t.TempDir()
	marker := filepath.Join(aside, "executed")
	script := filepath.Join(aside, "pre.sh")
	// The preprocessor has to pass the file through, or rg finds nothing and
	// the test cannot tell "blocked" from "broken".
	body := "#!/bin/sh\n: > " + marker + "\nexec cat \"$1\"\n"
	if err := os.WriteFile(script, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
	conf := filepath.Join(aside, "rgconf")
	if err := os.WriteFile(conf, []byte("--pre="+script+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	// The control. If this ripgrep does not honour --pre from its config file
	// — an old build, a distribution that compiled the feature out — then the
	// assertion below would pass without proving anything, and a skip says so
	// instead.
	probe := exec.Command("rg", "--no-heading", "--color", "never", "--regexp", "needle", "--", e.Cwd())
	probe.Env = append(os.Environ(), "RIPGREP_CONFIG_PATH="+conf)
	if out, err := probe.CombinedOutput(); err != nil {
		t.Skipf("the control run failed, so --pre cannot be shown to fire here: %v: %s", err, out)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Skip("this ripgrep does not run --pre from its config file; nothing to block")
	}
	if err := os.Remove(marker); err != nil {
		t.Fatal(err)
	}

	t.Setenv("RIPGREP_CONFIG_PATH", conf)

	matches, _, err := e.grepRipgrep(context.Background(), "needle", e.Cwd(), "", false, 100)
	if err != nil {
		t.Fatalf("grep through the rg backend failed: %v", err)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("grep ran the preprocessor named by RIPGREP_CONFIG_PATH: a repository that can set one environment variable executes code through a tool that never prompts")
	}
	// Ordinary use is unharmed: the search still works, it just is not
	// configurable by the tree.
	if !strings.Contains(joined(matches), "needle") {
		t.Fatalf("the rg backend stopped finding an ordinary match: %q", matches)
	}
}

// The environment variable must be gone from the child rather than merely
// unused, since rg reads it itself. Kept separate from the behavioural test
// above so that it still says something on a machine with no ripgrep at all.
func TestRipgrepEnvDropsItsConfigPath(t *testing.T) {
	t.Setenv("RIPGREP_CONFIG_PATH", "/tmp/whatever")
	t.Setenv("TAPIOCA_ORDINARY_VAR", "kept")

	var sawConfig, sawOther bool
	for _, kv := range rgEnv() {
		name, _, _ := strings.Cut(kv, "=")
		switch name {
		case "RIPGREP_CONFIG_PATH":
			sawConfig = true
		case "TAPIOCA_ORDINARY_VAR":
			sawOther = true
		}
	}
	if sawConfig {
		t.Fatal("RIPGREP_CONFIG_PATH reached the ripgrep child")
	}
	if !sawOther {
		t.Fatal("the rest of the environment was dropped too; rg needs PATH and friends")
	}
}
