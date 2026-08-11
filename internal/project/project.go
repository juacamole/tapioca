// Package project loads per-worktree context for the system prompt:
// committed instruction files (AGENTS.md, TAPIOCA.md) and personal memory
// added via /remember. Memory lives in the data dir keyed by worktree path —
// nothing is ever written into the project itself.
package project

import (
	"crypto/sha1"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"tapioca/internal/config"
	"tapioca/internal/textenc"
)

const (
	maxInstructions = 80_000
	maxMemory       = 40_000
)

// instructionFiles are the names read from each directory. The other tools'
// filenames are included deliberately: a repo that already documents itself
// for one agent should not have to repeat itself for this one.
var instructionFiles = []string{
	"AGENTS.md", "TAPIOCA.md", "CLAUDE.md", "GEMINI.md", "CRUSH.md",
}

const maxImportDepth = 3

// Instructions gathers project context for cwd: a global file first, then
// every instruction file from the repository root down to cwd. Working in a
// subdirectory has to pick up the repository's own instructions, and the
// nearest directory wins by coming last.
func Instructions(cwd string) string {
	var parts []string
	seen := map[string]bool{}
	dirs := ancestry(cwd)
	// dirs is outermost-first, so dirs[0] is the repository root.
	roots := importRoots{GlobalDir()}
	if len(dirs) > 0 {
		roots = append(roots, dirs[0])
	}

	add := func(path string) {
		abs, err := filepath.Abs(path)
		if err != nil || seen[abs] {
			return
		}
		seen[abs] = true
		text, ok := readInstruction(abs, seen, 0, roots)
		if !ok {
			return
		}
		parts = append(parts, fmt.Sprintf("[%s]\n%s", displayPath(abs, cwd), text))
	}

	for _, name := range instructionFiles {
		add(filepath.Join(GlobalDir(), name))
	}
	for _, dir := range dirs {
		for _, name := range instructionFiles {
			add(filepath.Join(dir, name))
		}
	}

	out := strings.Join(parts, "\n\n")
	if len(out) > maxInstructions {
		out = out[:maxInstructions] + "\n[truncated]"
	}
	return out
}

// GlobalDir is where instruction files that apply everywhere live.
func GlobalDir() string { return config.Dir() }

// ancestry lists cwd and its parents, outermost first, stopping at the
// repository root so a stray file in /home or / never joins every prompt.
func ancestry(cwd string) []string {
	abs, err := filepath.Abs(cwd)
	if err != nil {
		return nil
	}
	var dirs []string
	for dir := abs; ; {
		dirs = append(dirs, dir)
		if isRepoRoot(dir) {
			break
		}
		parent := filepath.Dir(dir)
		if parent == dir || parent == filepath.Dir(parent) {
			break // reached the filesystem root
		}
		dir = parent
	}
	// Collected innermost-first; the nearest file should be read last so it
	// has the final word.
	for i, j := 0, len(dirs)-1; i < j; i, j = i+1, j-1 {
		dirs[i], dirs[j] = dirs[j], dirs[i]
	}
	return dirs
}

func isRepoRoot(dir string) bool {
	for _, marker := range []string{".git", ".hg", ".jj"} {
		if _, err := os.Stat(filepath.Join(dir, marker)); err == nil {
			return true
		}
	}
	return false
}

// importRoots are the only directories an @import may read from: the project
// the instructions belong to, and the config directory where personal
// instruction files live.
//
// Instruction files are committed, so a cloned repository chooses what they
// say. Without this, "@~/.aws/credentials" in an AGENTS.md put the file into
// the system prompt — sent to the provider on every turn, in every mode
// including plan, saved into the session, with no tool call for the user to
// decline. Cloning the repository was the whole exploit.
type importRoots []string

func (r importRoots) allows(abs string) bool {
	real := resolveReal(abs)
	for _, root := range r {
		root = resolveReal(root)
		if real == root || strings.HasPrefix(real, root+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

// resolveReal follows symlinks, so a link committed inside the project cannot
// point an import outside it. A path that does not exist falls back to a
// lexical clean; the read then fails on its own.
func resolveReal(p string) string {
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	return filepath.Clean(p)
}

// readInstruction reads one file and resolves its @imports.
func readInstruction(path string, seen map[string]bool, depth int, roots importRoots) (string, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", false
	}
	text, ok := textenc.Decode(data)
	if !ok || strings.TrimSpace(text) == "" {
		return "", false
	}
	return strings.TrimSpace(expandImports(text, filepath.Dir(path), seen, depth, roots)), true
}

// expandImports replaces a line that is exactly "@path" with that file's
// contents, so a long instruction file can be split up. Cycles and runaway
// nesting are bounded by seen and depth; roots bound where it may read.
func expandImports(text, dir string, seen map[string]bool, depth int, roots importRoots) string {
	if depth >= maxImportDepth || !strings.Contains(text, "@") {
		return text
	}
	lines := strings.Split(text, "\n")
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "@") || len(trimmed) < 2 || strings.ContainsAny(trimmed, " \t") {
			continue
		}
		target := expandHome(strings.TrimPrefix(trimmed, "@"))
		if !filepath.IsAbs(target) {
			target = filepath.Join(dir, target)
		}
		abs, err := filepath.Abs(target)
		if err != nil || seen[abs] {
			lines[i] = ""
			continue
		}
		seen[abs] = true
		if !roots.allows(abs) {
			// Named rather than dropped: a silent refusal looks identical to a
			// typo, and this one is worth noticing. The path is left out on
			// purpose — it is attacker-chosen text.
			lines[i] = "[import refused: outside the project]"
			continue
		}
		if body, ok := readInstruction(abs, seen, depth+1, roots); ok {
			lines[i] = body
		} else {
			lines[i] = ""
		}
	}
	return strings.Join(lines, "\n")
}

func expandHome(p string) string {
	if strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, p[2:])
		}
	}
	return p
}

// displayPath labels a source file: bare name when it is in cwd, otherwise
// enough path to tell two AGENTS.md files apart.
func displayPath(abs, cwd string) string {
	if rel, err := filepath.Rel(cwd, abs); err == nil && !strings.HasPrefix(rel, "..") {
		return rel
	}
	if home, err := os.UserHomeDir(); err == nil && strings.HasPrefix(abs, home) {
		return "~" + strings.TrimPrefix(abs, home)
	}
	return abs
}

// MemoryPath returns where /remember facts for cwd are stored.
func MemoryPath(cwd string) string {
	sum := sha1.Sum([]byte(cwd))
	return filepath.Join(config.DataDir(), "memory", fmt.Sprintf("%x.md", sum[:8]))
}

// Memory returns the remembered facts for cwd, if any.
func Memory(cwd string) string {
	data, err := os.ReadFile(MemoryPath(cwd))
	if err != nil {
		return ""
	}
	s := strings.TrimSpace(string(data))
	if len(s) > maxMemory {
		s = s[len(s)-maxMemory:]
	}
	return s
}

// Remember appends a fact to cwd's memory.
func Remember(cwd, fact string) error {
	p := MemoryPath(cwd)
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(p, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = fmt.Fprintf(f, "- %s\n", strings.TrimSpace(fact))
	return err
}

// ClearMemory removes cwd's memory file.
func ClearMemory(cwd string) error {
	err := os.Remove(MemoryPath(cwd))
	if os.IsNotExist(err) {
		return nil
	}
	return err
}
