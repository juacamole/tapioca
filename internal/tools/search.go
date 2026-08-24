package tools

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"tapioca/internal/gitcmd"
	"tapioca/internal/secretenv"
	"tapioca/internal/textenc"
)

const (
	defaultMatches = 100
	maxMatches     = 400
	maxGlobResults = 300
	maxLineWidth   = 400
)

// skipDirs never contain source worth searching but are big enough to make a
// walk feel broken. ripgrep gets this from .gitignore; the fallback does not.
var skipDirs = map[string]bool{
	".git": true, "node_modules": true, "vendor": true, "target": true,
	"dist": true, "build": true, ".venv": true, "venv": true,
	"__pycache__": true, ".next": true, ".cache": true, ".idea": true,
	"result": true, ".direnv": true,
}

// trackedSet asks git which files it would consider under root: everything
// tracked plus untracked-but-not-ignored. ripgrep honours .gitignore and the
// plain walk cannot, so without this the same query answers differently
// depending on whether rg happens to be installed. nil means "no opinion"
// (not a repo, or no git), and the walk falls back to skipDirs alone.
func trackedSet(root string) map[string]bool {
	if _, err := exec.LookPath("git"); err != nil {
		return nil
	}
	out, err := gitcmd.In(root, "ls-files", "--cached", "--others",
		"--exclude-standard", "-z").Output()
	if err != nil {
		return nil
	}
	set := map[string]bool{}
	for _, rel := range strings.Split(string(out), "\x00") {
		if rel != "" {
			set[filepath.Join(root, rel)] = true
		}
	}
	return set
}

// searchRoot resolves the directory a search starts from, defaulting to cwd.
//
// The default is resolved too, and that is not cosmetic: filepath.WalkDir
// lstats its root, so a root that is itself a symlink is one non-directory
// entry and the walk descends into nothing. Working in a linked directory —
// /tmp on macOS, a stowed checkout, a relocated home — made the fallback
// answer "no matches" for the whole project. It went unseen because ripgrep
// follows the root it is given, so the bug only appears on a machine without
// it, which is also the machine that cannot afford a silently empty search.
func (e *Executor) searchRoot(path string) string {
	if strings.TrimSpace(path) == "" {
		return realPath(filepath.Clean(e.Cwd()))
	}
	return e.resolve(path)
}

func (e *Executor) grep(ctx context.Context, raw json.RawMessage) (string, bool, error) {
	var a struct {
		Pattern         string `json:"pattern"`
		Path            string `json:"path"`
		Glob            string `json:"glob"`
		CaseInsensitive bool   `json:"case_insensitive"`
		MaxResults      int    `json:"max_results"`
	}
	if err := json.Unmarshal(raw, &a); err != nil || strings.TrimSpace(a.Pattern) == "" {
		return "invalid arguments: need {\"pattern\": \"...\"}", true, nil
	}
	limit := a.MaxResults
	if limit <= 0 {
		limit = defaultMatches
	}
	if limit > maxMatches {
		limit = maxMatches
	}
	root := e.searchRoot(a.Path)
	if _, err := os.Stat(root); err != nil {
		return err.Error(), true, nil
	}

	matches, truncated, err := e.grepRipgrep(ctx, a.Pattern, root, a.Glob, a.CaseInsensitive, limit)
	if err != nil {
		matches, truncated, err = e.grepWalk(ctx, a.Pattern, root, a.Glob, a.CaseInsensitive, limit)
		if err != nil {
			return err.Error(), true, nil
		}
	}
	if len(matches) == 0 {
		return "no matches", false, nil
	}
	out := strings.Join(matches, "\n")
	if truncated {
		out += fmt.Sprintf("\n[... stopped at %d matches; narrow the pattern or set path/glob ...]", limit)
	}
	return capOutput(out), false, nil
}

// grepRipgrep shells out to rg when it is installed: it is far faster and
// honours .gitignore. An error here means "not usable", not "no matches".
func (e *Executor) grepRipgrep(ctx context.Context, pattern, root, glob string, insensitive bool, limit int) ([]string, bool, error) {
	bin, err := exec.LookPath("rg")
	if err != nil {
		return nil, false, err
	}
	args := []string{"--line-number", "--no-heading", "--color", "never",
		// The path is followed by a NUL instead of a colon. Filenames may
		// contain colons and, in an extracted tarball, the attacker picks them:
		// cutting the line at the first colon put the cut left of the filename
		// for anything under a directory called "a:b", so sensitivePath judged
		// "<root>/a" and waved through <root>/a:b/id_rsa. A NUL cannot occur in
		// a path, so this cut is the same one the walk makes.
		"--null",
		// Without a preview rg replaces an over-long line with a marker and no
		// content at all, where the walk clips it and returns the first
		// maxLineWidth characters — a match in a minified bundle or a one-line
		// JSON fixture came back empty on one backend and useful on the other.
		"--max-columns", strconv.Itoa(maxLineWidth), "--max-columns-preview",
		"--max-count", strconv.Itoa(limit)}
	// Dotfiles are ordinary project files — .github/workflows, .golangci.yml,
	// .tapioca/commands — and the walk searches them. rg skips them unless it
	// is told not to, so grep answered differently depending on whether rg
	// happened to be installed, and the machine that has it is the common one.
	args = append(args, "--hidden")
	// rg skips these only when .gitignore says so; the fallback always does,
	// and the two backends must not return different result sets. With
	// --hidden above, the .git entry is also what keeps rg out of the object
	// store and the reflog.
	for dir := range skipDirs {
		args = append(args, "--glob", "!"+dir+"/")
	}
	if insensitive {
		args = append(args, "--ignore-case")
	}
	if glob != "" {
		args = append(args, "--glob", glob)
	}
	args = append(args, "--regexp", pattern, "--", root)

	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Env = rgEnv()
	out, err := cmd.Output()
	// Exit 1 is "no matches"; anything above that is a real failure (bad
	// regex, unreadable root) and should fall through to the Go walk.
	if err != nil {
		if ee, ok := err.(*exec.ExitError); !ok || ee.ExitCode() != 1 {
			return nil, false, fmt.Errorf("rg: %w", err)
		}
	}
	var matches []string
	truncated := false
	sc := bufio.NewScanner(strings.NewReader(string(out)))
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		file, rest, ok := strings.Cut(sc.Text(), "\x00")
		if !ok || e.sensitivePath(file) {
			continue
		}
		if len(matches) >= limit {
			truncated = true
			break
		}
		matches = append(matches, e.relative(file)+":"+rest)
	}
	return matches, truncated, nil
}

// rgEnv is the scrubbed environment with ripgrep's own configuration channel
// taken out of it.
//
// rg reads a file of command-line arguments from $RIPGREP_CONFIG_PATH, and one
// of the arguments it accepts there is `--pre=COMMAND`: rg then runs COMMAND
// on every file it searches and greps the output. So two lines an extracted
// tarball ships — an .envrc exporting RIPGREP_CONFIG_PATH into the tree, and
// the file it names holding `--pre=./x.sh` — turn grep into arbitrary
// execution. grep is the wrong tool for that to be true of: it is
// non-mutating, so it never reaches the permission gate and never prompts, and
// it is available in plan mode, which means the tree runs code before the user
// has approved anything at all.
//
// Removing the variable rather than passing --no-config, because the flag is a
// version question: an rg old enough to reject it fails the whole search and
// silently drops grep onto the Go walk, and the environment is where the
// setting actually lives. Nothing legitimate points it at a repository either
// — a user's own rg config sits in their home directory and is theirs, not the
// tree's, but there is no way to tell those two apart from here, and losing a
// personal --smart-case is worth rather less than this.
func rgEnv() []string {
	src := secretenv.Scrubbed()
	out := make([]string, 0, len(src))
	for _, kv := range src {
		if name, _, ok := strings.Cut(kv, "="); ok && name == "RIPGREP_CONFIG_PATH" {
			continue
		}
		out = append(out, kv)
	}
	return out
}

// grepLines feeds path's lines to fn, numbered from 1, and stops when fn says
// to. It exists because the walk used to slurp every file it visited.
//
// read_file caps its read at 16 MiB and writes down why: a file the process
// cannot hold kills the TUI, and everything since the last save goes with it.
// grep had no cap at all, and it is the search tool that needs no approval in
// any mode — including plan mode, where nothing else can touch the disk. One
// grep over a tree holding a single 64 MiB file asked the allocator for 160 MB,
// and the file's size is the repository's to choose: a tarball ships that as a
// sparse member, and git stores 64 MiB of one repeated line as a few kilobytes.
//
// Files within the cap are decoded whole, exactly as before, so UTF-16 and
// Latin-1 trees still search the way they did. Past it the file is streamed a
// line at a time — which is what ripgrep does, and the reason no machine with
// rg installed ever saw this.
func grepLines(path string, fn func(n int, line string) bool) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return
	}
	if info.Size() <= maxReadBytes {
		data, err := io.ReadAll(io.LimitReader(f, maxReadBytes+1))
		if err != nil {
			return
		}
		content, isText := textenc.Decode(data)
		if !isText {
			return
		}
		for n, line := range strings.Split(content, "\n") {
			if !fn(n+1, line) {
				return
			}
		}
		return
	}
	head := make([]byte, 8<<10)
	n, _ := io.ReadFull(f, head)
	// A NUL anywhere in the head takes the file out: either Decode calls it
	// binary, or it calls it UTF-16, and a UTF-16 file cannot be scanned as
	// bytes — the pattern would never match and every "line" would be one NUL
	// away from the last. Under the cap that case is still decoded properly;
	// over it, "no matches" beats matches that mean nothing.
	if bytes.IndexByte(head[:n], 0) >= 0 {
		return
	}
	if _, isText := textenc.Decode(head[:n]); !isText {
		return
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return
	}
	sc := bufio.NewScanner(f)
	// A line longer than this ends the scan rather than growing a buffer to
	// hold it: a file with no newline in it is one line, and its length is the
	// whole point of the file for whoever wrote it.
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for i := 1; sc.Scan(); i++ {
		if !fn(i, sc.Text()) {
			return
		}
	}
}

func (e *Executor) grepWalk(ctx context.Context, pattern, root, glob string, insensitive bool, limit int) ([]string, bool, error) {
	if insensitive {
		pattern = "(?i)" + pattern
	}
	re, err := regexp.Compile(pattern)
	if err != nil {
		return nil, false, fmt.Errorf("invalid pattern: %w", err)
	}
	var matches []string
	truncated := false
	tracked := trackedSet(root)
	walkErr := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if d.IsDir() {
			if skipDirs[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() || e.sensitivePath(path) {
			return nil
		}
		if tracked != nil && !tracked[path] {
			return nil // git ignores it, and so does ripgrep
		}
		if glob != "" && !globMatch(glob, filepath.Base(path)) && !globMatch(glob, e.relative(path)) {
			return nil
		}
		stop := false
		grepLines(path, func(n int, line string) bool {
			if !re.MatchString(line) {
				return true
			}
			if len(matches) >= limit {
				truncated, stop = true, true
				return false
			}
			matches = append(matches, fmt.Sprintf("%s:%d:%s", e.relative(path), n, clipLine(line)))
			return true
		})
		if stop {
			return filepath.SkipAll
		}
		return nil
	})
	if walkErr != nil && ctx.Err() != nil {
		return nil, false, ctx.Err()
	}
	return matches, truncated, nil
}

func (e *Executor) globFiles(ctx context.Context, raw json.RawMessage) (string, bool, error) {
	var a struct {
		Pattern string `json:"pattern"`
		Path    string `json:"path"`
	}
	if err := json.Unmarshal(raw, &a); err != nil || strings.TrimSpace(a.Pattern) == "" {
		return "invalid arguments: need {\"pattern\": \"...\"}", true, nil
	}
	pattern := a.Pattern
	// A bare "*.go" means "anywhere below the root", which is what models
	// nearly always intend; "./*.go" still means the root only.
	if !strings.Contains(pattern, "/") {
		pattern = "**/" + pattern
	}
	root := e.searchRoot(a.Path)
	if _, err := os.Stat(root); err != nil {
		return err.Error(), true, nil
	}

	type hit struct {
		rel  string
		mod  int64
		size int64
	}
	var hits []hit
	tracked := trackedSet(root)
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if d.IsDir() {
			if skipDirs[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil || !d.Type().IsRegular() || e.sensitivePath(path) {
			return nil
		}
		if tracked != nil && !tracked[path] {
			return nil
		}
		if !globMatch(pattern, filepath.ToSlash(rel)) {
			return nil
		}
		info, statErr := d.Info()
		if statErr != nil {
			return nil
		}
		hits = append(hits, hit{e.relative(path), info.ModTime().Unix(), info.Size()})
		return nil
	})
	if err != nil {
		return err.Error(), true, nil
	}
	if len(hits) == 0 {
		return "no files match " + a.Pattern, false, nil
	}
	// Most recently modified first: in a coding session that is almost always
	// the file being worked on.
	sort.Slice(hits, func(i, j int) bool {
		if hits[i].mod != hits[j].mod {
			return hits[i].mod > hits[j].mod
		}
		return hits[i].rel < hits[j].rel
	})
	truncated := false
	if len(hits) > maxGlobResults {
		hits, truncated = hits[:maxGlobResults], true
	}
	var b strings.Builder
	for _, h := range hits {
		fmt.Fprintf(&b, "%s (%d bytes)\n", h.rel, h.size)
	}
	if truncated {
		fmt.Fprintf(&b, "[... stopped at %d files ...]\n", maxGlobResults)
	}
	return capOutput(strings.TrimRight(b.String(), "\n")), false, nil
}

// globMatch matches a slash-separated path against a pattern where ** spans
// any number of directories; filepath.Match handles a single segment.
func globMatch(pattern, path string) bool {
	return matchSegments(strings.Split(pattern, "/"), strings.Split(path, "/"))
}

func matchSegments(pat, seg []string) bool {
	for len(pat) > 0 {
		if pat[0] == "**" {
			if len(pat) == 1 {
				return true
			}
			for i := 0; i <= len(seg); i++ {
				if matchSegments(pat[1:], seg[i:]) {
					return true
				}
			}
			return false
		}
		if len(seg) == 0 {
			return false
		}
		if ok, err := filepath.Match(pat[0], seg[0]); err != nil || !ok {
			return false
		}
		pat, seg = pat[1:], seg[1:]
	}
	return len(seg) == 0
}

// relative shortens paths inside the working directory so output stays
// readable and pasteable back into other tools.
//
// Both spellings of the working directory are tried. searchRoot resolves the
// path argument but not the default, so a grep given a path walks resolved
// paths while this compares against the working directory as written — and
// under a working directory reached through a link (`cd ~/src` where ~/src is
// one, /tmp on macOS) nothing was ever inside it. Every result then printed as
// an absolute path, and grep's own glob filter, which matches against this,
// stopped matching a pattern with a directory in it.
func (e *Executor) relative(path string) string {
	cwd := e.Cwd()
	if rel, err := filepath.Rel(cwd, path); err == nil && !strings.HasPrefix(rel, "..") {
		return rel
	}
	if real := realPath(filepath.Clean(cwd)); real != cwd && real != unresolvable {
		if rel, err := filepath.Rel(real, path); err == nil && !strings.HasPrefix(rel, "..") {
			return rel
		}
	}
	return path
}

func clipLine(s string) string {
	s = strings.TrimRight(s, "\r")
	if len(s) > maxLineWidth {
		return s[:maxLineWidth] + "…"
	}
	return s
}
