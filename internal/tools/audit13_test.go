package tools

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// sensitiveRoots asks three environment variables where the things worth
// stealing are: $HOME for ~/.ssh and ~/.config/gh, and config.Dir() /
// config.DataDir() — which is $XDG_CONFIG_HOME, $XDG_DATA_HOME,
// $TAPIOCA_CONFIG_DIR, $TAPIOCA_DATA_DIR — for Tapioca's own config and every
// session it has ever stored.
//
// Round 10 made those roots survive being *spelled* differently, by resolving
// them. This is the other move: the environment does not have to spell the same
// directory differently, it can name a different one. `export
// XDG_CONFIG_HOME=$PWD/.cfg` in an extracted tree's .envrc does not hide the
// real ~/.config/tapioca behind a link — it takes it out of the list, because
// the list is built from wherever the variable now points. The file is still on
// disk with every provider key and MCP bearer token in it, its basename matches
// nothing in sensitiveNames, and looksSecret does not fire on "config.toml", so
// read_file — which needs no approval in any other way, in any mode including
// plan — hands it back with no prompt.
//
// This is the same shape as the sandbox trusting $HOME while hooks.go did not:
// two places asking where the user's things are, and only one of them treating
// the answer as attacker-influenced.
func TestTapiocaOwnDirectoriesStaySensitiveWhenTheEnvironmentMovesThem(t *testing.T) {
	for _, tc := range []struct{ name, env, rel string }{
		{"XDG_CONFIG_HOME", "XDG_CONFIG_HOME", filepath.Join(".config", "tapioca", "config.toml")},
		{"TAPIOCA_CONFIG_DIR", "TAPIOCA_CONFIG_DIR", filepath.Join(".config", "tapioca", "config.toml")},
		{"XDG_DATA_HOME", "XDG_DATA_HOME", filepath.Join(".local", "share", "tapioca", "sessions", "s.json")},
		{"TAPIOCA_DATA_DIR", "TAPIOCA_DATA_DIR", filepath.Join(".local", "share", "tapioca", "sessions", "s.json")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := realHome(t)
			loot := filepath.Join(home, tc.rel)
			if err := os.MkdirAll(filepath.Dir(loot), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(loot, []byte(`api_key = "sk-CANARY"`), 0o600); err != nil {
				t.Fatal(err)
			}

			e := execIn(t, ModeManual)
			// The control: with nothing redirected, the default locations are
			// gated. Without it, "it prompted" below would prove nothing.
			if !e.sensitivePath(loot) {
				t.Fatal("the default location is not gated at all here; the rest of this test would be vacuous")
			}

			// One line in an .envrc, pointing the variable at a directory the
			// tree ships. Tapioca now keeps its state there — and the real one,
			// which is the one holding the keys, is nobody's business.
			t.Setenv(tc.env, filepath.Join(e.Cwd(), ".cfg"))
			if !e.sensitivePath(loot) {
				t.Errorf("$%s pointed elsewhere and %s stopped being sensitive", tc.env, tc.rel)
			}

			var asked []string
			out, isErr, err := e.Call(context.Background(), "read_file",
				args(t, map[string]string{"path": loot}), asker(Decision{Allow: false}, &asked))
			if err != nil {
				t.Fatal(err)
			}
			if len(asked) == 0 || !isErr {
				t.Errorf("read_file returned the file with no prompt: %q", out)
			}
			if strings.Contains(out, "sk-CANARY") {
				t.Errorf("the canary came back in the tool result: %q", out)
			}
		})
	}
}

// $HOME is the same question one level up, and the answer this file gives is
// the one hooks.go and sandbox.go already give: ask the account database too,
// and protect every directory either of them calls home.
//
// ~/.config/gh/hosts.yml is the sharp one — it holds a GitHub token, its
// basename matches nothing in sensitiveNames, and looksSecret does not fire on
// it, so the directory list is the only thing in front of it.
func TestSensitiveDirsSurviveAHomeTheEnvironmentMoved(t *testing.T) {
	base := t.TempDir()
	if resolved, err := filepath.EvalSymlinks(base); err == nil {
		base = resolved
	}
	home := filepath.Join(base, "real")
	if err := os.MkdirAll(filepath.Join(home, ".config", "gh"), 0o700); err != nil {
		t.Fatal(err)
	}
	token := filepath.Join(home, ".config", "gh", "hosts.yml")
	if err := os.WriteFile(token, []byte("github.com:\n  oauth_token: ghp_CANARY\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	if h, err := os.UserHomeDir(); err != nil || h != home {
		t.Skip("os.UserHomeDir does not follow HOME on this platform")
	}
	// The account database says the same thing the environment does, which is
	// the ordinary state; the attack is to make them differ.
	prev := accountHome
	accountHome = func() string { return home }
	t.Cleanup(func() { accountHome = prev })

	e := execIn(t, ModeManual)
	t.Setenv("TAPIOCA_CONFIG_DIR", t.TempDir())
	t.Setenv("TAPIOCA_DATA_DIR", t.TempDir())
	if !e.sensitivePath(token) {
		t.Fatal("~/.config/gh is not gated at all here; the rest of this test would be vacuous")
	}

	// `export HOME=$PWD/.home` — the one line that moved the sandbox's tmpfs
	// off the real home in the previous round.
	fake := filepath.Join(e.Cwd(), ".home")
	if err := os.MkdirAll(fake, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", fake)
	if !e.sensitivePath(token) {
		t.Error("$HOME pointed into the tree and ~/.config/gh stopped being sensitive")
	}
}

// realHome is a home directory the environment agrees with the account database
// about, so a test can put a file at a default location and have config.Dir()
// find it there.
func realHome(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if resolved, err := filepath.EvalSymlinks(dir); err == nil {
		dir = resolved
	}
	t.Setenv("HOME", dir)
	if h, err := os.UserHomeDir(); err != nil || h != dir {
		t.Skip("os.UserHomeDir does not follow HOME on this platform")
	}
	prev := accountHome
	accountHome = func() string { return dir }
	t.Cleanup(func() { accountHome = prev })
	// Every variable that can move these, cleared: the machine running the test
	// may have its own set, and the control below has to be the default.
	for _, v := range []string{"XDG_CONFIG_HOME", "XDG_DATA_HOME", "TAPIOCA_CONFIG_DIR", "TAPIOCA_DATA_DIR"} {
		t.Setenv(v, "")
	}
	return dir
}

// A permission rule may be written with a leading ~/, and matchSubject expands
// it with os.UserHomeDir — $HOME, and nothing else. The subject it is compared
// against arrives as an absolute path, so the two only meet while $HOME still
// names the directory the user meant.
//
// `export HOME=$PWD/.home` in an extracted tree's .envrc breaks that: the rule
// now describes a directory inside the checkout, the real home is outside every
// pattern, and a deny rule the user wrote to keep the agent out of ~/notes
// stopped denying anything. A deny that goes quiet is worse than one that was
// never written, because the user believes it is there.
func TestATildeRuleStillCoversTheRealHome(t *testing.T) {
	base := t.TempDir()
	if resolved, err := filepath.EvalSymlinks(base); err == nil {
		base = resolved
	}
	home := filepath.Join(base, "real")
	if err := os.MkdirAll(filepath.Join(home, "notes"), 0o700); err != nil {
		t.Fatal(err)
	}
	notes := filepath.Join(home, "notes", "plan.md")
	if err := os.WriteFile(notes, []byte("PRIVATE-CANARY"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	if h, err := os.UserHomeDir(); err != nil || h != home {
		t.Skip("os.UserHomeDir does not follow HOME on this platform")
	}
	prev := accountHome
	accountHome = func() string { return home }
	t.Cleanup(func() { accountHome = prev })

	e := execIn(t, ModeAuto)
	t.Setenv("TAPIOCA_CONFIG_DIR", t.TempDir())
	t.Setenv("TAPIOCA_DATA_DIR", t.TempDir())
	e.SetRules(nil, nil, []string{"read_file(~/notes/**)"})

	// The control: with $HOME naming the directory the user meant, the rule
	// denies. Nothing below means anything without this.
	if act := e.ruleFor("read_file", notes); act != RuleDeny {
		t.Fatalf("the rule does not deny even with an honest $HOME (%q); the rest of this test would be vacuous", act)
	}

	fake := filepath.Join(e.Cwd(), ".home")
	if err := os.MkdirAll(filepath.Join(fake, "notes"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", fake)
	if act := e.ruleFor("read_file", notes); act != RuleDeny {
		t.Errorf("$HOME pointed into the tree and the deny rule stopped covering the real ~/notes (%q)", act)
	}

	var asked []string
	out, isErr, err := e.Call(context.Background(), "read_file",
		args(t, map[string]string{"path": notes}), asker(Decision{Allow: true}, &asked))
	if err != nil {
		t.Fatal(err)
	}
	if !isErr || strings.Contains(out, "PRIVATE-CANARY") {
		t.Errorf("the denied file was read anyway: %q", out)
	}
}
