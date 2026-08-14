package ui

import (
	"os"
	"path/filepath"
	"time"

	tea "charm.land/bubbletea/v2"

	"tapioca/internal/config"
)

// Reloading already worked and worked well — leaving /settings re-read the
// file and applied all of it in place. What was missing was any other way to
// trigger it: an edit made in another window did nothing until you opened
// /settings and quit out of it again, and a command file the agent had just
// written was unusable until you did the same.
//
// The watching is stat polling rather than a filesystem watcher, for the
// reasons internal/tools/watch.go already gives: no descriptor limits, no
// missed events across editors that write via rename, and nothing to tear
// down. Config files are edited by hand a few times an hour, so a poll is
// generous.

// reloadPollInterval is how often the watched files are stat'd. Slow enough to
// be free, fast enough that saving a file and switching back to the app feels
// immediate.
const reloadPollInterval = 2 * time.Second

type reloadTickMsg struct{}

func reloadTick() tea.Cmd {
	return tea.Tick(reloadPollInterval, func(time.Time) tea.Msg { return reloadTickMsg{} })
}

// watchedPaths are the files and directories a reload would pick up. Missing
// entries are normal — most of these are optional — and a path that appears
// later is noticed by the same comparison that notices a change.
func (m *App) watchedPaths() []string {
	paths := []string{m.cfg.Path()}
	for _, dir := range m.commandDirs() {
		paths = append(paths, dir)
		if entries, err := os.ReadDir(dir); err == nil {
			for _, e := range entries {
				paths = append(paths, filepath.Join(dir, e.Name()))
			}
		}
	}
	return paths
}

// reloadStamp is a cheap fingerprint of everything watched. Size and modtime
// together catch the edits that matter; a change that alters neither is not
// one a config file makes.
func (m *App) reloadStamp() string {
	var b []byte
	for _, p := range m.watchedPaths() {
		st, err := os.Stat(p)
		if err != nil {
			b = append(b, (p + "\x00-\x00")...)
			continue
		}
		b = append(b, (p + "\x00" + st.ModTime().UTC().Format(time.RFC3339Nano) + "\x00" +
			formatInt(st.Size()) + "\x00")...)
	}
	return string(b)
}

func formatInt(n int64) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}

// noteReloadStamps records the current state, so a reload we performed is not
// then detected as an external change and performed again.
func (m *App) noteReloadStamps() { m.reloadSeen = m.reloadStamp() }

// checkReload reloads if anything watched has changed since it was last seen.
func (m *App) checkReload() tea.Cmd {
	if m.cfg == nil {
		return nil
	}
	now := m.reloadStamp()
	if now == m.reloadSeen {
		return nil
	}
	// Record first. A file that fails to parse must not be retried every two
	// seconds — the reload reports the error once, and the next real edit is
	// what tries again.
	m.reloadSeen = now
	if m.applyReload() {
		m.setFlash("reloaded — config changed on disk", false)
	}
	return m.flashCmd()
}

// commandDirs lists where user slash commands live, project first.
func (m *App) commandDirs() []string {
	var dirs []string
	if d := config.Dir(); d != "" {
		dirs = append(dirs, filepath.Join(d, "commands"))
	}
	dirs = append(dirs, filepath.Join(m.cwd(), ".tapioca", "commands"))
	return dirs
}

// cmdReload re-reads everything now, without opening an editor.
func cmdReload(m *App, _ string) tea.Cmd {
	if m.applyReload() {
		m.setFlash("reloaded", false)
	}
	return m.flashCmd()
}
