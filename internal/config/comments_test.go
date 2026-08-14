package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The app rewrites the file on every toggle, so a single shift+tab used to
// delete every explanation in it. The starter config is now a pointer to
// config.example.toml rather than a copy of it, so this starts from what a
// user has after copying the lines they wanted out of that example.
func TestSaveKeepsCommentsFromTheStarterConfig(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	if err := WriteDefault(path); err != nil {
		t.Fatal(err)
	}
	// Lines copied from the shipped example, comments and all.
	copied := `# Tapioca configuration.
permission_mode = "manual"      # plan | manual | auto | bypass
auto_compact = true             # summarize old turns when context nears the limit
`
	if err := os.WriteFile(path, []byte(copied), 0o600); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	commentsBefore := strings.Count(string(before), "#")

	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	cfg.PermissionMode = "auto" // what shift+tab does
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}

	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	text := string(after)
	if !strings.Contains(text, `permission_mode = "auto"`) {
		t.Fatalf("the change was not saved:\n%s", text)
	}
	if got := strings.Count(text, "#"); got < commentsBefore/2 {
		t.Errorf("comments dropped: %d before, %d after", commentsBefore, got)
	}
	for _, want := range []string{
		"plan | manual | auto | bypass",
		"summarize old turns when context nears the limit",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("lost the comment %q", want)
		}
	}
	// It must still be loadable, and the round trip must be stable.
	reloaded, err := Load(path)
	if err != nil {
		t.Fatalf("the saved file no longer parses: %v", err)
	}
	if reloaded.PermissionMode != "auto" {
		t.Errorf("permission_mode came back as %q", reloaded.PermissionMode)
	}
}

func TestSaveKeepsUserWrittenComments(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	original := `# my notes at the top
default_provider = "ollama"   # I use the local one
# reminder: bump this when the model gets bigger
max_tokens = 8192

[dashboard]
position = "left"   # I am left handed
`
	if err := os.WriteFile(path, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	cfg.MaxTokens = 4096
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	text, _ := os.ReadFile(path)
	for _, want := range []string{
		"# my notes at the top",
		"# I use the local one",
		"# reminder: bump this when the model gets bigger",
		"# I am left handed",
		"max_tokens = 4096",
	} {
		if !strings.Contains(string(text), want) {
			t.Errorf("missing %q in:\n%s", want, text)
		}
	}
}

// A hex colour is a value, not a comment; treating '#' inside quotes as one
// would truncate the setting.
func TestHashInsideStringsIsNotAComment(t *testing.T) {
	code, comment := splitComment(`accent = "#A78BFA"   # violet`)
	if code != `accent = "#A78BFA"` || comment != "# violet" {
		t.Fatalf("code=%q comment=%q", code, comment)
	}
	code, comment = splitComment(`accent = "#A78BFA/#6C4FD8"`)
	if comment != "" || !strings.Contains(code, "#6C4FD8") {
		t.Fatalf("split a value with no comment: code=%q comment=%q", code, comment)
	}
}

func TestSaveOfColorsSurvivesRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte("[colors]\naccent = \"#FF0066\"  # loud\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	reloaded, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.Colors["accent"] != "#FF0066" {
		t.Errorf("colour mangled on save: %q", reloaded.Colors["accent"])
	}
	text, _ := os.ReadFile(path)
	if !strings.Contains(string(text), "# loud") {
		t.Errorf("comment lost:\n%s", text)
	}
}

// Comments must be stable under repeated saves: drift or duplication would
// grow the file a little on every toggle.
func TestRepeatedSavesDoNotDriftComments(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := WriteDefault(path); err != nil {
		t.Fatal(err)
	}
	annotated := `# Tapioca configuration.
permission_mode = "manual"      # plan | manual | auto | bypass
max_tokens = 4096               # max output tokens per response
`
	if err := os.WriteFile(path, []byte(annotated), 0o600); err != nil {
		t.Fatal(err)
	}
	var sizes []int
	var texts []string
	for i, mode := range []string{"auto", "plan", "manual", "auto"} {
		cfg, err := Load(path)
		if err != nil {
			t.Fatalf("save %d made the file unparsable: %v", i, err)
		}
		cfg.PermissionMode = mode
		if err := cfg.Save(); err != nil {
			t.Fatal(err)
		}
		text, _ := os.ReadFile(path)
		sizes = append(sizes, strings.Count(string(text), "#"))
		texts = append(texts, string(text))
	}
	for i := 1; i < len(sizes); i++ {
		if sizes[i] != sizes[0] {
			t.Fatalf("comment count drifted: %v", sizes)
		}
	}
	// The first and last saves set the same mode, so the file should be
	// byte-identical despite three rewrites in between.
	if texts[0] != texts[3] {
		t.Error("the file is not stable across identical saves")
	}
	if !strings.Contains(texts[3], "plan | manual | auto | bypass") {
		t.Error("the documentation did not survive four rewrites")
	}
}
