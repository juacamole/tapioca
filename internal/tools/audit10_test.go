package tools

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A dotfiles setup is the ordinary state of the directories this gate protects:
// `~/.config` stowed out of a repository, a home directory a corporate image
// links elsewhere, XDG_CONFIG_HOME pointed at a versioned directory. Every one
// of those puts a symlink in the middle of the path.
//
// sensitivePath compares the path being judged — which resolve() has already
// put through realPath, so it is fully resolved — against roots taken straight
// from config.Dir(), config.DataDir() and $HOME. A resolved path compared
// against an unresolved root cannot match once any component of the root is a
// link, so the whole check went quiet exactly where it was needed: read_file
// handed back config.toml, with every provider key and MCP bearer token in it,
// with no prompt in any mode. inWorkArea in the same file resolves its roots
// for precisely this reason.
func TestConfigDirReachedThroughALinkIsStillSensitive(t *testing.T) {
	home := t.TempDir()
	// Where the files really live, and the ~/.config that points at them.
	store := filepath.Join(home, "dotfiles")
	if err := os.MkdirAll(filepath.Join(store, "tapioca"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(store, filepath.Join(home, ".config")); err != nil {
		t.Skipf("cannot create a symlink here: %v", err)
	}
	linked := filepath.Join(home, ".config", "tapioca")
	keys := filepath.Join(linked, "config.toml")
	if err := os.WriteFile(keys, []byte(`api_key = "sk-CANARY"`), 0o600); err != nil {
		t.Fatal(err)
	}

	e := execIn(t, ModeManual)
	t.Setenv("TAPIOCA_DATA_DIR", t.TempDir())

	// The control: name the directory by the path with no link in it and the
	// gate fires. Without this, "it prompted" below could be true for a reason
	// that has nothing to do with the link.
	t.Setenv("TAPIOCA_CONFIG_DIR", filepath.Join(store, "tapioca"))
	if !e.sensitivePath(keys) {
		t.Fatal("the config directory is not gated at all here; the rest of this test would be vacuous")
	}

	// The same directory, named through the link — which is what config.Dir()
	// returns for a stowed ~/.config, with or without the variable set.
	t.Setenv("TAPIOCA_CONFIG_DIR", linked)
	if !e.sensitivePath(keys) {
		t.Error("config.toml under a symlinked config directory did not read as sensitive")
	}
	var asked []string
	out, isErr, err := e.Call(context.Background(), "read_file",
		args(t, map[string]string{"path": keys}), asker(Decision{Allow: false}, &asked))
	if err != nil {
		t.Fatal(err)
	}
	if len(asked) == 0 || !isErr {
		t.Errorf("the provider keys were read with no prompt: %q", out)
	}
}

// The same question about the well-known dot directories: they are joined onto
// $HOME and compared unresolved. A home directory reached through a link — the
// macOS /Users case, an automounted or relocated home — took every one of them
// out of the gate. ~/.config/gh/hosts.yml holds a GitHub token and its
// basename matches nothing in sensitiveNames, so the directory list is the
// only thing standing in front of it.
func TestSensitiveDirsUnderALinkedHomeAreStillSensitive(t *testing.T) {
	base := t.TempDir()
	realHome := filepath.Join(base, "real")
	if err := os.MkdirAll(filepath.Join(realHome, ".config", "gh"), 0o755); err != nil {
		t.Fatal(err)
	}
	linkedHome := filepath.Join(base, "home")
	if err := os.Symlink(realHome, linkedHome); err != nil {
		t.Skipf("cannot create a symlink here: %v", err)
	}
	token := filepath.Join(linkedHome, ".config", "gh", "hosts.yml")
	if err := os.WriteFile(token, []byte("github.com:\n  oauth_token: ghp_CANARY\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	e := execIn(t, ModeManual)
	t.Setenv("TAPIOCA_CONFIG_DIR", t.TempDir())
	t.Setenv("TAPIOCA_DATA_DIR", t.TempDir())

	// The control: with HOME naming the directory directly, the gate fires.
	t.Setenv("HOME", realHome)
	if h, err := os.UserHomeDir(); err != nil || h != realHome {
		t.Skip("os.UserHomeDir does not follow HOME on this platform")
	}
	if !e.sensitivePath(token) {
		t.Fatal("~/.config/gh is not gated at all here; the rest of this test would be vacuous")
	}

	t.Setenv("HOME", linkedHome)
	if !e.sensitivePath(token) {
		t.Error("~/.config/gh under a home reached through a link did not read as sensitive")
	}
}

// A permission rule's path pattern is matched against three spellings of the
// call's subject, and the third — the pattern made absolute against the working
// directory — was built on the working directory as written while the subject
// it is compared to has been resolved. A working directory reached through a
// link is ordinary: `cd ~/src` where ~/src is a link leaves that spelling in
// $PWD and os.Getwd hands it back, /tmp is a link on macOS, and /cd stores
// whatever was typed. So the comparison could not match, and a deny rule
// written relative to the project stopped denying as soon as the model spelled
// the path absolutely — which it does, because the absolute working directory
// is in its system prompt.
func TestARelativeDenyRuleHoldsUnderASymlinkedCwd(t *testing.T) {
	base := t.TempDir()
	real := filepath.Join(base, "real")
	if err := os.MkdirAll(filepath.Join(real, "secrets"), 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("cannot create a symlink here: %v", err)
	}
	if err := os.WriteFile(filepath.Join(real, "secrets", "key.txt"), []byte("CANARY"), 0o600); err != nil {
		t.Fatal(err)
	}

	e := NewExecutor(link, ModeManual)
	e.SetRules(nil, nil, []string{"read_file(secrets/**)"})

	// The control: the rule is loaded and does deny the spelling it is written
	// in. Without this the assertions below would pass for a config that never
	// parsed.
	if act := e.RuleFor("read_file", "secrets/key.txt"); act != RuleDeny {
		t.Fatalf("the rule does not even deny the relative spelling: %q", act)
	}

	// The working directory as the model is told it, joined with the same path.
	if act := e.RuleFor("read_file", filepath.Join(link, "secrets", "key.txt")); act != RuleDeny {
		t.Errorf("deny did not match %s (the cwd the model is given): %q",
			filepath.Join(link, "secrets", "key.txt"), act)
	}
	// And the resolved spelling, which is what grep output and `realpath` give.
	if act := e.RuleFor("read_file", filepath.Join(real, "secrets", "key.txt")); act != RuleDeny {
		t.Errorf("deny did not match the resolved path %s: %q",
			filepath.Join(real, "secrets", "key.txt"), act)
	}
}

// The same defect the other way round, which is a defect of its own: an allow
// rule that stops applying makes an ordinary edit prompt. A user who wrote
// allow = ["edit_file(internal/**)"] to stop being asked is asked anyway, for
// every edit the model spells absolutely, as soon as the checkout is reached
// through a link.
func TestARelativeAllowRuleStillAppliesUnderASymlinkedCwd(t *testing.T) {
	base := t.TempDir()
	real := filepath.Join(base, "real")
	if err := os.MkdirAll(filepath.Join(real, "internal"), 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("cannot create a symlink here: %v", err)
	}

	e := NewExecutor(link, ModeManual)
	e.SetRules([]string{"edit_file(internal/**)"}, nil, nil)

	if act := e.RuleFor("edit_file", "internal/x.go"); act != RuleAllow {
		t.Fatalf("the rule does not even allow the relative spelling: %q", act)
	}
	if act := e.RuleFor("edit_file", filepath.Join(link, "internal", "x.go")); act != RuleAllow {
		t.Errorf("allow stopped applying to %s: an ordinary edit now prompts",
			filepath.Join(link, "internal", "x.go"))
	}
}

// Ordinary use, broken the same way: searchRoot resolves the path argument and
// not the default, so grep given a path walks resolved paths while relative()
// compared them against the working directory as written. Under a symlinked
// working directory every result printed as an absolute path, and the tool's
// own glob filter — which matches against that same string — stopped matching
// any pattern with a directory in it.
func TestGrepStaysRelativeUnderASymlinkedCwd(t *testing.T) {
	base := t.TempDir()
	real := filepath.Join(base, "real")
	if err := os.MkdirAll(filepath.Join(real, "internal"), 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("cannot create a symlink here: %v", err)
	}
	if err := os.WriteFile(filepath.Join(real, "internal", "x.go"), []byte("package internal // NEEDLE\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	e := NewExecutor(link, ModeBypass)
	t.Setenv("TAPIOCA_CONFIG_DIR", t.TempDir())
	t.Setenv("TAPIOCA_DATA_DIR", t.TempDir())

	// The control: with no path argument the root is the working directory as
	// written and the answer is already relative, so this is about the branch
	// that resolves.
	out, isErr, err := e.Call(context.Background(), "grep",
		args(t, map[string]string{"pattern": "NEEDLE"}), asker(Decision{Allow: true}, new([]string)))
	if err != nil || isErr {
		t.Fatalf("plain grep failed: %v %q", err, out)
	}
	if !strings.HasPrefix(out, "internal/x.go:") {
		t.Fatalf("grep without a path is not relative either: %q", out)
	}

	out, isErr, err = e.Call(context.Background(), "grep",
		args(t, map[string]string{"pattern": "NEEDLE", "path": "internal"}), asker(Decision{Allow: true}, new([]string)))
	if err != nil || isErr {
		t.Fatalf("grep with a path failed: %v %q", err, out)
	}
	if !strings.HasPrefix(out, "internal/x.go:") {
		t.Errorf("grep with a path printed an absolute result under a symlinked cwd: %q", out)
	}
}
