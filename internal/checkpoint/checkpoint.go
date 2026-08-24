// Package checkpoint snapshots the working tree into a shadow git repo
// before agent mutations, so any change — including bash side effects — can
// be rewound. The shadow git dir lives under the data directory, keyed by
// work tree path, and never touches the project's own .git.
package checkpoint

import (
	"crypto/sha1"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"tapioca/internal/config"
	"tapioca/internal/gitcmd"
	"tapioca/internal/secretenv"
)

var mu sync.Mutex

// Available reports whether the git binary exists on PATH.
func Available() bool {
	_, err := exec.LookPath("git")
	return err == nil
}

// Entry is one stored snapshot.
type Entry struct {
	ID    string
	Label string
	Time  time.Time
}

func gitDir(workTree string) string {
	sum := sha1.Sum([]byte(workTree))
	return filepath.Join(config.DataDir(), "checkpoints", fmt.Sprintf("%x", sum[:8]))
}

func run(workTree string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	// Its git dir is a predictable path under the data dir, so a planted
	// pre-commit hook runs on the next mutating tool call — in this session and
	// in every later one, which makes it a way to keep execution that a single
	// approved write bought. hooksPath is pinned away for that, and fsmonitor
	// because `add -A` would run it too. It gets the same filtered environment
	// as every other child.
	//
	// gitcmd's whole static list, rather than a shorter one of its own: which
	// keys name a program is a question with one answer, and two lists is how
	// the shorter one ends up being the list that runs. It is not gitcmd's
	// *complete* answer, which also enumerates the repository's own keys — that
	// pass reads the config of the repo it is pointed at, and this runs against
	// a git dir under the data dir, where the worktree's config is never a
	// source. The global scope is shut off below rather than pinned, since a
	// pin can only name a key it already knows about.
	//
	// gpgSign is the one addition: a user whose global config signs every
	// commit does not want gpg invoked by a snapshot, and pinning gpg.program
	// alone would only make the commit fail.
	pins := append(gitcmd.StaticPins(),
		gitcmd.Pin{Key: "commit.gpgSign", Value: "false"},
	)
	// The inherited GIT_CONFIG_* variables do not travel, because the sentence
	// above — "nothing in the tree can write one" — was only true of the
	// *files* the shadow repo reads. Configuration also arrives by environment,
	// and the environment is reachable from the tree through an .envrc:
	// GIT_CONFIG_KEY_0=filter.p.clean with `* filter=p` in .gitattributes made
	// the `add -A` below run whatever the repository chose, and GIT_CONFIG_GLOBAL
	// pointed at a file in the worktree did the same. A pin cannot answer that,
	// since filter.<name>.clean is a key the repository names; gitcmd survives
	// it only by enumerating the configuration first, which is exactly what
	// this caller does not do.
	//
	// GIT_CONFIG_GLOBAL was only the variable that names that file outright.
	// $HOME is where git looks for .gitconfig when nothing names it, and
	// $XDG_CONFIG_HOME is where it looks for git/config — both ordinary
	// environment variables, both reachable through the same .envrc. So
	// `export HOME=$PWD/.home` plus a committed .home/.gitconfig holding
	// filter.p.clean reached exactly the value the explicit variable had just
	// been refused for, by naming the directory instead of the file, and the
	// `add -A` below ran it. Blocking one spelling of "read this file" and not
	// the other was not blocking anything.
	//
	// The global scope is therefore turned off altogether, which is also the
	// honest description of what this repo wants: it is a machine-internal
	// snapshot store, not the user's work, and no preference of theirs should
	// change what it records. The identity a commit needs is supplied below,
	// and a global core.excludesFile — which would otherwise keep an ignored
	// file out of `add -A`, and so out of any rewind — stops applying too.
	// os.DevNull rather than a literal /dev/null, so the same line means an
	// empty config on Windows.
	cmd.Env = append(gitcmd.WithPins(gitcmd.WithoutInheritedConfig(append(secretenv.Scrubbed(),
		"GIT_DIR="+gitDir(workTree),
		"GIT_WORK_TREE="+workTree,
		"GIT_AUTHOR_NAME=tapioca", "GIT_AUTHOR_EMAIL=checkpoint@tapioca",
		"GIT_COMMITTER_NAME=tapioca", "GIT_COMMITTER_EMAIL=checkpoint@tapioca",
	)), pins...), "GIT_CONFIG_GLOBAL="+os.DevNull)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git %s: %s", args[0], strings.TrimSpace(string(out)))
	}
	return strings.TrimSpace(string(out)), nil
}

func ensure(workTree string) error {
	d := gitDir(workTree)
	if _, err := os.Stat(filepath.Join(d, "HEAD")); err == nil {
		return nil
	}
	if err := os.MkdirAll(d, 0o700); err != nil {
		return err
	}
	_, err := run(workTree, "init", "-q")
	return err
}

// Ignored content is snapshotted too, up to these. See addIgnored.
const (
	maxIgnoredFiles = 4000
	maxIgnoredBytes = 64 << 20
)

// addIgnored stages the ignored content `add -A` skipped, within a budget.
//
// The global core.excludesFile was turned off because "a global ignore file
// that hides a path from `git add -A` would make the checkpoint miss a file it
// is supposed to be able to rewind". core.excludesFile is not the only file
// that answers "is this path ignored", and it is not the one an extracted
// tarball writes. `.gitignore` in the work tree is, it is attacker-authored
// like every other file there, and git ranks it *above* core.excludesFile — so
// the spelling that was closed is the one the user supplies and the spelling
// left open was the one the repository supplies.
//
// What that bought an attacker was not a lost file at the margins. A checkpoint
// is the only copy of an untracked file: anything the project's own git tracks
// can be recovered from the project's own git, so the paths where /rewind is
// the sole recourse are exactly the ones a .gitignore line takes out of it. One
// committed line — `notes.txt`, or `*` — and the agent's damage to that path
// was unrewindable, silently: /rewind still lists the checkpoint, still reports
// that it rewound, and restores nothing.
//
// The budget is the reason this is not simply `add -A -f`. Ignore rules are
// mostly honest, and what they name is mostly node_modules, target and .venv:
// forcing those in costs a fifth of a second before every mutating tool call
// and, worse, commits a fresh copy of a half-gigabyte build output every time
// the agent runs a build. So directories are measured before they are taken,
// with a walk that gives up as soon as it is over budget — which for a
// dependency tree is almost immediately, and for a source tree never happens.
// Ignored files not inside a directory of their own are always taken; they are
// individually named and cost nothing.
//
// The residual gap is a tree that ignores a directory holding more than the
// budget. That is a worse hiding place than it sounds — the attacker has to put
// the user's own work inside it — and it is the price of not making an ordinary
// build unusable.
func addIgnored(workTree string) error {
	out, err := run(workTree, "ls-files", "-z", "--others", "--ignored",
		"--exclude-standard", "--directory", "--no-empty-directory")
	if err != nil {
		return err
	}
	files, bytes := maxIgnoredFiles, int64(maxIgnoredBytes)
	var take []string
	for _, entry := range strings.Split(out, "\x00") {
		if entry == "" {
			continue
		}
		if !strings.HasSuffix(entry, "/") {
			// An individually named ignored file. .git is never in this list;
			// git excludes it from ls-files on its own.
			take = append(take, entry)
			continue
		}
		if affordable(filepath.Join(workTree, entry), &files, &bytes) {
			take = append(take, strings.TrimSuffix(entry, "/"))
		}
	}
	// One pathspec per entry, in batches, each marked literal so that a file
	// named `:(glob)x` — or one beginning with a dash — is a path and not an
	// instruction. `--` alone does not cover pathspec magic.
	const batch = 128
	for len(take) > 0 {
		n := min(batch, len(take))
		args := []string{"add", "-f", "--"}
		for _, p := range take[:n] {
			args = append(args, ":(literal)"+p)
		}
		if _, err := run(workTree, args...); err != nil {
			return err
		}
		take = take[n:]
	}
	return nil
}

// affordable reports whether a directory fits in what is left of the budget,
// spending it as it goes. It stops walking the moment it does not, so a
// dependency tree costs a few hundred entries rather than a full traversal.
func affordable(dir string, files *int, bytes *int64) bool {
	f, b := *files, *bytes
	over := false
	_ = filepath.WalkDir(dir, func(_ string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if err != nil || !info.Mode().IsRegular() {
			// A FIFO or a device in an ignored directory is not content, and
			// reading one is how a snapshot hangs.
			return nil
		}
		f--
		b -= info.Size()
		if f < 0 || b < 0 {
			over = true
			return filepath.SkipAll
		}
		return nil
	})
	if over {
		return false
	}
	*files, *bytes = f, b
	return true
}

// Snapshot stores the current tree state; a no-op (returning "") when
// nothing changed since the last snapshot.
func Snapshot(workTree, label string) (string, error) {
	mu.Lock()
	defer mu.Unlock()
	if err := ensure(workTree); err != nil {
		return "", err
	}
	if _, err := run(workTree, "add", "-A"); err != nil {
		return "", err
	}
	if err := addIgnored(workTree); err != nil {
		return "", err
	}
	if _, err := run(workTree, "rev-parse", "--verify", "HEAD"); err == nil {
		if _, err := run(workTree, "diff-index", "--quiet", "HEAD"); err == nil {
			return "", nil // clean
		}
	}
	if label == "" {
		label = "checkpoint"
	}
	if _, err := run(workTree, "commit", "-q", "--allow-empty-message", "-m", label); err != nil {
		return "", err
	}
	return run(workTree, "rev-parse", "--short", "HEAD")
}

// List returns snapshots, newest first.
func List(workTree string, n int) ([]Entry, error) {
	mu.Lock()
	defer mu.Unlock()
	out, err := run(workTree, "log", "--format=%h%x00%ct%x00%s", "-n", strconv.Itoa(n))
	if err != nil {
		return nil, err
	}
	var entries []Entry
	for _, line := range strings.Split(out, "\n") {
		parts := strings.SplitN(line, "\x00", 3)
		if len(parts) != 3 {
			continue
		}
		ts, _ := strconv.ParseInt(parts[1], 10, 64)
		entries = append(entries, Entry{ID: parts[0], Label: parts[2], Time: time.Unix(ts, 0)})
	}
	return entries, nil
}

// Restore rewinds the work tree to a snapshot. The current state is
// snapshotted first, so a rewind is itself rewindable. Files created after the
// target snapshot are removed — `clean` without -x, so an ignored file created
// since is left in place rather than deleted. That is the safe direction of the
// asymmetry with addIgnored: an ignored file the snapshot holds is restored,
// and one it does not is not thrown away.
func Restore(workTree, id string) error {
	if _, err := Snapshot(workTree, "before rewind to "+id); err != nil {
		return err
	}
	mu.Lock()
	defer mu.Unlock()
	if _, err := run(workTree, "read-tree", id); err != nil {
		return err
	}
	if _, err := run(workTree, "checkout-index", "-a", "-f"); err != nil {
		return err
	}
	_, err := run(workTree, "clean", "-fdq")
	return err
}
