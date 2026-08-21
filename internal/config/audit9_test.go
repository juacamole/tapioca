package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// base_url is refused to an in-tree config because it decides where the
// conversation and the credential go. credentials_file makes the same decision
// one indirection further out: the vertex service-account grant reads token_uri
// out of the JSON that file names and POSTs to it, so a repository committing a
// config and a key file beside it chose the address — and the exchange happens
// on its own the first time a model answers.
func TestAnInTreeConfigCannotChooseAProvidersCredentialsFile(t *testing.T) {
	tree := t.TempDir()
	if err := os.WriteFile(filepath.Join(tree, "go.mod"), []byte("module x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(tree, "config.toml")
	const file = "[providers.vx]\n  type = \"vertex\"\n  project = \"p\"\n" +
		"  credentials_file = \"./sa.json\"\n"
	if err := os.WriteFile(path, []byte(file), 0o600); err != nil {
		t.Fatal(err)
	}

	// The control: from outside the tree this is the user's own config and the
	// key is honoured, so the assertion below is about the location and not
	// about the key never being read.
	outside, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if notes := outside.RestrictIfInsideTree(t.TempDir()); len(notes) != 0 {
		t.Fatalf("restricted from outside the tree: %v", notes)
	}
	if outside.Providers["vx"].CredentialsFile == "" {
		t.Fatal("the control lost credentials_file before the test began")
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	notes := cfg.RestrictIfInsideTree(tree)
	if cfg.Providers["vx"].CredentialsFile != "" {
		t.Errorf("credentials_file survived: %q", cfg.Providers["vx"].CredentialsFile)
	}
	found := false
	for _, n := range notes {
		if strings.Contains(n, "credentials_file") {
			found = true
		}
	}
	if !found {
		t.Errorf("nothing was said about it: %v", notes)
	}
}

// A key withdrawn for this run must still be in the file afterwards: changing
// a setting in the app must not delete the repository's own config keys, and
// base_url already worked that way.
func TestARestrictedCredentialsFileIsNotDeletedFromTheFile(t *testing.T) {
	tree := t.TempDir()
	if err := os.WriteFile(filepath.Join(tree, "go.mod"), []byte("module x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(tree, "config.toml")
	const file = "[providers.vx]\n  type = \"vertex\"\n  project = \"p\"\n" +
		"  credentials_file = \"./sa.json\"\n"
	if err := os.WriteFile(path, []byte(file), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	cfg.RestrictIfInsideTree(tree)
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	back, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if back.Providers["vx"].CredentialsFile != "./sa.json" {
		t.Errorf("saving deleted the repository's own credentials_file: %q",
			back.Providers["vx"].CredentialsFile)
	}
}
