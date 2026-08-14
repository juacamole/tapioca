// Package skills loads SKILL.md capability packs: instructions the model pulls
// in when it judges a task needs them. A slash command is a macro the user
// fires; a skill is something the model reaches for on its own, so only the
// name and description of each one are in the system prompt and the body stays
// on disk until load_skill asks for it. Packs live in .tapioca/skills or in the
// config directory, the same two places custom commands come from.
package skills

import (
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"tapioca/internal/config"
	"tapioca/internal/textenc"
)

const (
	skillDir    = "skills"
	projectDir  = ".tapioca"
	manifest    = "SKILL.md"
	maxManifest = 64 * 1024
	// maxDesc bounds one catalogue line. Descriptions are paid for on every
	// turn of every conversation, whether or not the skill is ever used, so the
	// budget is roughly a sentence — about 50 tokens.
	maxDesc = 200
	// maxSkills bounds the catalogue as a whole; a directory with a thousand
	// packs in it would otherwise be a system prompt nobody asked for.
	maxSkills = 100
	// maxFiles bounds the bundled listing. It exists so the model knows a
	// checklist or a script is there, not to index a vendored tree.
	maxFiles = 40
	maxName  = 64
)

// Skill is one capability pack. There is no body field on purpose: what this
// struct holds is what the model sees every turn.
type Skill struct {
	Name        string
	Description string
	Dir         string // resolved, so nothing read from it leaves the pack
	Project     bool   // from the worktree rather than the config directory
}

// Problem is a pack that could not be used. Skills are hand-written and arrive
// with cloned repositories, so a broken one is ordinary; it is named and
// skipped rather than allowed to take the startup down with it.
type Problem struct {
	Path   string
	Reason string
}

// Load reads both skill directories, project last so it wins by name, and
// returns the catalogue plus whatever was skipped.
func Load(cwd string) ([]Skill, []Problem) {
	byName := map[string]Skill{}
	var probs []Problem

	read := func(dir string, project bool) {
		entries, err := os.ReadDir(dir)
		if err != nil {
			return
		}
		root, err := filepath.EvalSymlinks(dir)
		if err != nil {
			return
		}
		kept, broken := 0, 0
		for _, e := range entries {
			if kept >= maxSkills || broken >= maxSkills {
				return
			}
			// A dotted directory is .git or an editor's leftovers, never a pack
			// somebody forgot the manifest in, so it is not worth reporting.
			if strings.HasPrefix(e.Name(), ".") {
				continue
			}
			path := filepath.Join(dir, e.Name())
			real, err := filepath.EvalSymlinks(path)
			// A link is followed only while it stays in the skills directory:
			// skills/x -> /home/you would otherwise put a stranger's files one
			// load_skill away, and the manifest names the directory the model is
			// then told to read from.
			if err != nil || filepath.Dir(real) != root {
				continue
			}
			if info, err := os.Stat(real); err != nil || !info.IsDir() {
				continue // a stray README in the skills directory is not a pack
			}
			s, prob := parse(real, e.Name())
			if prob != nil {
				probs = append(probs, *prob)
				broken++
				continue
			}
			s.Project = project
			byName[s.Name] = s
			kept++
		}
	}
	read(filepath.Join(config.Dir(), skillDir), false)
	read(filepath.Join(cwd, projectDir, skillDir), true)

	out := make([]Skill, 0, len(byName))
	for _, s := range byName {
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, probs
}

// Find looks up one pack by name.
func Find(cwd, name string) (Skill, bool) {
	name = strings.ToLower(strings.TrimSpace(name))
	list, _ := Load(cwd)
	for _, s := range list {
		if s.Name == name {
			return s, true
		}
	}
	return Skill{}, false
}

// Catalog is the block that sits in the system prompt: one line per skill, and
// nothing else.
func Catalog(list []Skill) string {
	var b strings.Builder
	for _, s := range list {
		fmt.Fprintf(&b, "- %s: %s\n", s.Name, s.Description)
	}
	return strings.TrimRight(b.String(), "\n")
}

// parse reads one pack's manifest. dir is already resolved; name is the
// directory as it was written, which is the skill's name unless the front
// matter overrides it.
func parse(dir, name string) (Skill, *Problem) {
	fail := func(reason string) (Skill, *Problem) {
		return Skill{}, &Problem{Path: filepath.Join(dir, manifest), Reason: reason}
	}
	real, err := manifestPath(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return fail("no " + manifest)
		}
		return fail(err.Error())
	}
	data, err := readCapped(real, maxManifest+1)
	if err != nil {
		return fail("unreadable")
	}
	if len(data) > maxManifest {
		return fail(fmt.Sprintf("larger than %dKB", maxManifest/1024))
	}
	text, ok := textenc.Decode(data)
	if !ok {
		return fail("not text")
	}
	front, body, ok := splitFront(text)
	if !ok {
		return fail("no --- front matter with a description")
	}
	// The directory names the skill, and a front-matter name overrides it —
	// packs written for other tools carry one, and the model must be told the
	// name their own description refers to.
	name = cleanName(name)
	if n := cleanName(front["name"]); n != "" {
		name = n
	}
	if name == "" {
		return fail("name is not usable as a skill name (a-z, 0-9, - and _)")
	}
	desc := cleanDesc(front["description"])
	if desc == "" {
		return fail("no description in the front matter")
	}
	if strings.TrimSpace(body) == "" {
		return fail("front matter only — there is nothing to load")
	}
	return Skill{Name: name, Description: desc, Dir: dir}, nil
}

// manifestPath resolves a pack's SKILL.md. A link out of the pack is refused
// wherever the read happens: the file is read with nobody approving it and its
// text goes to the model, which is the same exploit as a command file pointing
// at ~/.aws/credentials.
func manifestPath(dir string) (string, error) {
	real, err := filepath.EvalSymlinks(filepath.Join(dir, manifest))
	if err != nil {
		return "", err
	}
	if filepath.Dir(real) != dir {
		return "", fmt.Errorf("%s links outside the skill directory", manifest)
	}
	return real, nil
}

// splitFront separates the "---" front matter from the body. It is not YAML
// and does not try to be: a catalogue entry needs a key and a value, and a
// parser dependency to read two lines would be a poor trade. Unterminated
// front matter is refused rather than guessed at, since guessing turns the
// whole file into metadata and puts it in every system prompt.
func splitFront(text string) (map[string]string, string, bool) {
	lines := strings.Split(text, "\n")
	if len(lines) == 0 || strings.TrimSpace(lines[0]) != "---" {
		return nil, "", false
	}
	front := map[string]string{}
	for i := 1; i < len(lines); i++ {
		if strings.TrimSpace(lines[i]) == "---" {
			return front, strings.Join(lines[i+1:], "\n"), true
		}
		k, v, ok := strings.Cut(lines[i], ":")
		if !ok {
			continue
		}
		front[strings.ToLower(strings.TrimSpace(k))] = unquote(strings.TrimSpace(v))
	}
	return nil, "", false
}

func unquote(s string) string {
	if len(s) >= 2 && (s[0] == '"' || s[0] == '\'') && s[len(s)-1] == s[0] {
		return s[1 : len(s)-1]
	}
	return s
}

// cleanName returns "" for anything that is not a plain name. The result is
// what the model passes to load_skill and what labels the loaded text, so a
// name carrying spaces, separators or control characters is refused rather
// than repaired into something the author did not write.
func cleanName(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	if s == "" || len(s) > maxName {
		return ""
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
		default:
			return ""
		}
	}
	if s == "." || s == ".." {
		return ""
	}
	return s
}

// cleanDesc bounds one catalogue line. The text comes from a repository, and
// it is rendered in the TUI as well as sent to the model, so control
// characters go before either sees them.
func cleanDesc(s string) string {
	s = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f || (r >= 0x80 && r <= 0x9f) {
			return ' '
		}
		return r
	}, s)
	s = strings.TrimSpace(strings.Join(strings.Fields(s), " "))
	if len(s) > maxDesc {
		s = strings.TrimSpace(textenc.Cut(s, maxDesc)) + "…"
	}
	return s
}

// Header labels loaded skill text. The model loading a skill and the user
// loading one produce the same line, so a single pass over a transcript
// answers what is in context.
func Header(name string) string { return "[skill: " + name + "]" }

// HeaderName reports which skill a block of loaded text belongs to. Only a
// leading header counts: the marker is a fact about how the text got there,
// and text quoting it later did not come from a load.
func HeaderName(text string) (string, bool) {
	line, _, _ := strings.Cut(strings.TrimSpace(text), "\n")
	line = strings.TrimSpace(line)
	if !strings.HasPrefix(line, "[skill: ") || !strings.HasSuffix(line, "]") {
		return "", false
	}
	name := strings.TrimSuffix(strings.TrimPrefix(line, "[skill: "), "]")
	if name = cleanName(name); name == "" {
		return "", false
	}
	return name, true
}

// Body returns the pack as the model receives it: the header, where its files
// are, and the instructions. This is the expensive half of the skill and the
// only part read on demand.
func (s Skill) Body() (string, error) {
	real, err := manifestPath(s.Dir)
	if err != nil {
		return "", err
	}
	data, err := readCapped(real, maxManifest)
	if err != nil {
		return "", err
	}
	text, ok := textenc.Decode(data)
	if !ok {
		return "", fmt.Errorf("%s is not text", manifest)
	}
	_, body, ok := splitFront(text)
	if !ok {
		return "", fmt.Errorf("%s changed on disk and no longer parses", manifest)
	}
	var b strings.Builder
	b.WriteString(Header(s.Name) + "\n")
	b.WriteString("directory: " + s.Dir + "\n")
	if files := s.Files(); len(files) > 0 {
		b.WriteString("files: " + strings.Join(files, ", ") + " (read or run them from that directory)\n")
	}
	b.WriteString("\n" + strings.TrimSpace(body) + "\n")
	return b.String(), nil
}

// Files lists what travels with the pack, so a checklist or a helper script is
// known to be there without a search for it. Anything that resolves out of the
// pack is left out: this listing is an invitation to read what it names.
func (s Skill) Files() []string {
	var out []string
	_ = filepath.WalkDir(s.Dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if len(out) >= maxFiles {
			return fs.SkipAll
		}
		if strings.HasPrefix(d.Name(), ".") {
			if d.IsDir() && path != s.Dir {
				return fs.SkipDir
			}
			return nil
		}
		if d.IsDir() || d.Name() == manifest {
			return nil
		}
		real, err := filepath.EvalSymlinks(path)
		if err != nil || !strings.HasPrefix(real, s.Dir+string(filepath.Separator)) {
			return nil
		}
		rel, err := filepath.Rel(s.Dir, path)
		if err != nil {
			return nil
		}
		out = append(out, filepath.ToSlash(rel))
		return nil
	})
	sort.Strings(out)
	return out
}

func readCapped(path string, limit int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	if info, err := f.Stat(); err == nil && info.IsDir() {
		return nil, fmt.Errorf("%s is a directory", path)
	}
	return io.ReadAll(io.LimitReader(f, limit))
}
