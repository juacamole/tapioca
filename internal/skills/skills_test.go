package skills

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const release = "---\nname: release\ndescription: Cut a release end to end.\n---\n\nBump the version, then tag it.\n"

// writeSkill creates one pack and returns its directory.
func writeSkill(t *testing.T, root, name, manifestText string) string {
	t.Helper()
	dir := filepath.Join(root, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, manifest), []byte(manifestText), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

// personalRoot points the config directory at a temporary one and returns the
// skills directory inside it.
func personalRoot(t *testing.T) string {
	t.Helper()
	cfg := t.TempDir()
	t.Setenv("TAPIOCA_CONFIG_DIR", cfg)
	return filepath.Join(cfg, skillDir)
}

func projectRoot(t *testing.T, work string) string {
	t.Helper()
	return filepath.Join(work, projectDir, skillDir)
}

func TestLoadsFromBothPlaces(t *testing.T) {
	personal := personalRoot(t)
	work := t.TempDir()
	writeSkill(t, personal, "review", "---\ndescription: Review a diff.\n---\nLook for missing tests.\n")
	writeSkill(t, projectRoot(t, work), "release", release)

	list, probs := Load(work)
	if len(probs) != 0 {
		t.Fatalf("unexpected problems: %+v", probs)
	}
	if len(list) != 2 {
		t.Fatalf("loaded %d skills: %+v", len(list), list)
	}
	if list[0].Name != "release" || !list[0].Project {
		t.Errorf("the worktree pack should be first and marked project: %+v", list[0])
	}
	if list[1].Name != "review" || list[1].Project {
		t.Errorf("the config-directory pack should be personal: %+v", list[1])
	}
	if list[1].Description != "Review a diff." {
		t.Errorf("description not read: %q", list[1].Description)
	}
}

// The same precedence custom commands have: the worktree decides.
func TestProjectSkillsOverridePersonalOnes(t *testing.T) {
	personal := personalRoot(t)
	work := t.TempDir()
	writeSkill(t, personal, "release", "---\ndescription: personal version\n---\npersonal body\n")
	writeSkill(t, projectRoot(t, work), "release", "---\ndescription: project version\n---\nproject body\n")

	list, _ := Load(work)
	if len(list) != 1 {
		t.Fatalf("expected one skill, got %+v", list)
	}
	if list[0].Description != "project version" || !list[0].Project {
		t.Errorf("personal version won: %+v", list[0])
	}
}

// A pack that does not parse must be named and skipped. Startup reads these
// files, and a hand-written manifest in a cloned repository is the normal case,
// not the exceptional one.
func TestMalformedManifestsAreReportedAndSkipped(t *testing.T) {
	cases := []struct {
		name string
		text string
	}{
		{"nofront", "Just some notes with no front matter.\n"},
		{"unterminated", "---\ndescription: never closed\nstill going\n"},
		{"nodesc", "---\nname: nodesc\n---\nbody\n"},
		{"emptydesc", "---\ndescription:    \n---\nbody\n"},
		{"nobody", "---\ndescription: front matter only\n---\n\n"},
		{"huge", "---\ndescription: too big\n---\n" + strings.Repeat("x", maxManifest+1)},
	}
	work := t.TempDir()
	root := projectRoot(t, work)
	personalRoot(t)
	for _, c := range cases {
		writeSkill(t, root, c.name, c.text)
	}
	if err := os.MkdirAll(filepath.Join(root, "nomanifest"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeSkill(t, root, "release", release)

	list, probs := Load(work)
	if len(list) != 1 || list[0].Name != "release" {
		t.Fatalf("a broken pack took a good one with it: %+v", list)
	}
	if len(probs) != len(cases)+1 {
		t.Fatalf("expected %d problems, got %+v", len(cases)+1, probs)
	}
	for _, p := range probs {
		if p.Reason == "" || p.Path == "" {
			t.Errorf("a skipped pack was not named: %+v", p)
		}
	}
}

// The manifest is read without anyone approving it and its text goes to the
// model, so a link out of the skills directory is the commands exploit again:
// skills/x/SKILL.md -> ~/.config/tapioca/config.toml.
func TestManifestCannotBeASymlinkOut(t *testing.T) {
	personalRoot(t)
	work := t.TempDir()
	secret := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(secret, []byte("---\ndescription: x\n---\napi_key = \"sk-CANARY\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(projectRoot(t, work), "leak")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(secret, filepath.Join(dir, manifest)); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	list, probs := Load(work)
	for _, s := range list {
		body, err := s.Body()
		if err == nil && strings.Contains(body, "CANARY") {
			t.Fatalf("%s serves a file from outside the skills directory", s.Name)
		}
	}
	if len(list) != 0 || len(probs) != 1 {
		t.Fatalf("the linked manifest was not refused: %+v %+v", list, probs)
	}
}

// The directory itself is the other half of that: skills/x -> /home/you would
// make every file under it something load_skill points the model at.
func TestSkillDirectoryCannotBeASymlinkOut(t *testing.T) {
	personalRoot(t)
	work := t.TempDir()
	outside := t.TempDir()
	writeSkill(t, outside, "release", release)
	root := projectRoot(t, work)
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "release"), filepath.Join(root, "release")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	if list, _ := Load(work); len(list) != 0 {
		t.Fatalf("a pack outside the skills directory was loaded: %+v", list)
	}
}

// Descriptions are paid for on every turn whether or not the skill is used, and
// they are drawn in the TUI, so neither length nor control characters are the
// manifest author's decision.
func TestDescriptionIsBoundedAndSingleLine(t *testing.T) {
	personalRoot(t)
	work := t.TempDir()
	writeSkill(t, projectRoot(t, work), "wide",
		"---\ndescription: \"\x1b[31mred\a "+strings.Repeat("long ", 200)+"\"\n---\nbody\n")

	list, _ := Load(work)
	if len(list) != 1 {
		t.Fatalf("expected one skill: %+v", list)
	}
	desc := list[0].Description
	if len(desc) > maxDesc+8 {
		t.Errorf("description not bounded: %d bytes", len(desc))
	}
	if strings.ContainsAny(desc, "\x1b\x07") {
		t.Errorf("control characters survived: %q", desc)
	}
}

// What load_skill returns: the instructions, where they live, and what came
// with them — but not the front matter, which the model already has.
func TestBodyCarriesHeaderFilesAndInstructions(t *testing.T) {
	personalRoot(t)
	work := t.TempDir()
	dir := writeSkill(t, projectRoot(t, work), "release", release)
	if err := os.WriteFile(filepath.Join(dir, "checklist.md"), []byte("1. tests\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "bin", "verify.sh"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	list, _ := Load(work)
	body, err := list[0].Body()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(body, Header("release")) {
		t.Errorf("no leading header, so nothing can tell what is loaded: %q", body)
	}
	for _, want := range []string{"Bump the version", "checklist.md", "bin/verify.sh", list[0].Dir} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q from the loaded skill: %q", want, body)
		}
	}
	if strings.Contains(body, "description: Cut a release") {
		t.Errorf("front matter sent twice: %q", body)
	}
}

// The listing tells the model to go and read what it names.
func TestFilesLeavesOutLinksOutOfTheSkill(t *testing.T) {
	personalRoot(t)
	work := t.TempDir()
	secret := filepath.Join(t.TempDir(), "id_rsa")
	if err := os.WriteFile(secret, []byte("KEY"), 0o600); err != nil {
		t.Fatal(err)
	}
	dir := writeSkill(t, projectRoot(t, work), "release", release)
	if err := os.Symlink(secret, filepath.Join(dir, "key.txt")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	list, _ := Load(work)
	if files := list[0].Files(); len(files) != 0 {
		t.Errorf("a link out of the pack was advertised: %v", files)
	}
}

// The header is a fact about how text arrived, so only a leading one counts —
// a model that writes "[skill: release]" in a sentence has loaded nothing.
func TestHeaderNameOnlyMatchesALeadingHeader(t *testing.T) {
	if name, ok := HeaderName(Header("release") + "\ndirectory: /x\n"); !ok || name != "release" {
		t.Errorf("leading header not recognised: %q %v", name, ok)
	}
	for _, text := range []string{
		"I will now use [skill: release] for this",
		"[skill: ../../etc/passwd]",
		"[skill: ]",
		"nothing here",
	} {
		if name, ok := HeaderName(text); ok {
			t.Errorf("%q counted as a load of %q", text, name)
		}
	}
}

func TestNoSkillsDirectoryIsFine(t *testing.T) {
	personalRoot(t)
	list, probs := Load(t.TempDir())
	if len(list) != 0 || len(probs) != 0 {
		t.Errorf("expected nothing, got %+v %+v", list, probs)
	}
}

// A directory that is not a pack is not a mistake to report: .git and a stray
// README both live happily next to real skills.
func TestStrayEntriesAreIgnoredSilently(t *testing.T) {
	personalRoot(t)
	work := t.TempDir()
	root := projectRoot(t, work)
	writeSkill(t, root, "release", release)
	if err := os.MkdirAll(filepath.Join(root, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "README.md"), []byte("packs live here"), 0o644); err != nil {
		t.Fatal(err)
	}

	list, probs := Load(work)
	if len(list) != 1 || len(probs) != 0 {
		t.Fatalf("got %+v %+v", list, probs)
	}
}

// The catalogue is the whole cost of an unused skill.
func TestCatalogIsOneLinePerSkill(t *testing.T) {
	personalRoot(t)
	work := t.TempDir()
	writeSkill(t, projectRoot(t, work), "release", release)
	list, _ := Load(work)
	cat := Catalog(list)
	if strings.Count(cat, "\n") != 0 {
		t.Errorf("one skill produced more than one line: %q", cat)
	}
	if !strings.Contains(cat, "release") || !strings.Contains(cat, "Cut a release end to end.") {
		t.Errorf("catalogue line is missing name or description: %q", cat)
	}
	if strings.Contains(cat, "Bump the version") {
		t.Errorf("the body reached the catalogue: %q", cat)
	}
}

// Find is what load_skill resolves a name with; it must not be a path.
func TestFindIsByNameOnly(t *testing.T) {
	personalRoot(t)
	work := t.TempDir()
	writeSkill(t, projectRoot(t, work), "release", release)
	if _, ok := Find(work, "release"); !ok {
		t.Error("a loaded skill was not found by name")
	}
	for _, name := range []string{"../release", "release/../release", "RELEASE/", "missing"} {
		if s, ok := Find(work, name); ok {
			t.Errorf("%q resolved to %+v", name, s)
		}
	}
}
