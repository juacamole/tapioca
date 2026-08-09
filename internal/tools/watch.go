package tools

import (
	"os"
	"sort"
	"time"
)

// fileStamp is what the agent last saw of a file. Polling stat when it matters
// beats a filesystem watcher here: no descriptor limits, no missed events
// across editors that write via rename, and nothing to tear down.
type fileStamp struct {
	mod  time.Time
	size int64
}

func stampOf(path string) (fileStamp, bool) {
	info, err := os.Stat(path)
	if err != nil || info.IsDir() {
		return fileStamp{}, false
	}
	return fileStamp{mod: info.ModTime(), size: info.Size()}, true
}

// note records the state of a file the agent just read or wrote.
func (e *Executor) note(path string) {
	st, ok := stampOf(path)
	if !ok {
		return
	}
	e.mu.Lock()
	if e.seen == nil {
		e.seen = map[string]fileStamp{}
	}
	e.seen[path] = st
	e.mu.Unlock()
}

// changedExternally reports whether a tracked file differs from what the agent
// last saw. Untracked files are not "changed" — the agent has no stale picture
// of them to correct.
func (e *Executor) changedExternally(path string) bool {
	e.mu.Lock()
	prev, tracked := e.seen[path]
	e.mu.Unlock()
	if !tracked {
		return false
	}
	now, ok := stampOf(path)
	if !ok {
		return false // deleted; the tool call will fail on its own terms
	}
	return now != prev
}

// staleFileMsg refuses a write that would discard someone else's edit. The
// wording tells the model the one thing that fixes it.
func staleFileMsg(path string) string {
	return path + " changed on disk since you last read it — someone edited it " +
		"outside Tapioca. Read it again before writing, or your change will " +
		"discard theirs."
}

// ChangedFiles returns tracked files edited outside Tapioca since the agent
// last saw them, and re-stamps them so each change is reported once.
func (e *Executor) ChangedFiles() []string {
	e.mu.Lock()
	paths := make([]string, 0, len(e.seen))
	for p := range e.seen {
		paths = append(paths, p)
	}
	e.mu.Unlock()

	var changed []string
	for _, p := range paths {
		if e.changedExternally(p) {
			changed = append(changed, e.relative(p))
			e.note(p)
		}
	}
	sort.Strings(changed)
	return changed
}
