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
)

var mu sync.Mutex

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
	cmd.Env = append(os.Environ(),
		"GIT_DIR="+gitDir(workTree),
		"GIT_WORK_TREE="+workTree,
		"GIT_AUTHOR_NAME=tapioca", "GIT_AUTHOR_EMAIL=checkpoint@tapioca",
		"GIT_COMMITTER_NAME=tapioca", "GIT_COMMITTER_EMAIL=checkpoint@tapioca",
	)
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
	if err := os.MkdirAll(d, 0o755); err != nil {
		return err
	}
	_, err := run(workTree, "init", "-q")
	return err
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
// snapshotted first, so a rewind is itself rewindable. Files created after
// the target snapshot are removed (gitignored files are left alone).
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
