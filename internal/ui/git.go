package ui

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"tapioca/internal/agent"
	"tapioca/internal/gitcmd"
)

// gitInfo is a cached snapshot of the working directory's git state, shown by
// the "git" and "changes" panels.
type gitInfo struct {
	ok         bool
	branch     string
	ahead      int
	behind     int
	lastCommit string
	files      []gitFile
}

type gitFile struct {
	x, y byte // porcelain status columns (staged, unstaged)
	path string
}

type gitStatusMsg struct{ info gitInfo }
type gitTickMsg struct{}

func gitTick() tea.Cmd {
	return tea.Tick(5*time.Second, func(time.Time) tea.Msg { return gitTickMsg{} })
}

// gitPanelsVisible reports whether any git-backed panel is on screen.
func (m *App) gitPanelsVisible() bool {
	if !m.cfg.Dashboard.Visible {
		return false
	}
	for _, p := range m.cfg.Dashboard.Panels {
		if p == "git" || p == "changes" {
			return true
		}
	}
	return false
}

// fetchGitCmd gathers git status in the background.
func fetchGitCmd(cwd string) tea.Cmd {
	return func() tea.Msg {
		out, err := gitcmd.In(cwd, "status", "--porcelain", "-b").Output()
		if err != nil {
			return gitStatusMsg{gitInfo{}}
		}
		info := gitInfo{ok: true}
		for _, line := range strings.Split(strings.TrimRight(string(out), "\n"), "\n") {
			if strings.HasPrefix(line, "## ") {
				head := strings.TrimPrefix(line, "## ")
				if i := strings.Index(head, "..."); i >= 0 {
					info.branch = head[:i]
					if j := strings.Index(head, "["); j >= 0 {
						for _, part := range strings.Split(strings.Trim(head[j:], "[]"), ",") {
							part = strings.TrimSpace(part)
							if n, ok := strings.CutPrefix(part, "ahead "); ok {
								info.ahead, _ = strconv.Atoi(n)
							}
							if n, ok := strings.CutPrefix(part, "behind "); ok {
								info.behind, _ = strconv.Atoi(n)
							}
						}
					}
				} else {
					info.branch = strings.TrimSuffix(head, " (no branch)")
				}
				continue
			}
			if len(line) < 4 {
				continue
			}
			path := strings.TrimSpace(line[3:])
			if i := strings.Index(path, " -> "); i >= 0 { // renames: show target
				path = path[i+4:]
			}
			info.files = append(info.files, gitFile{x: line[0], y: line[1], path: path})
		}
		if out, err := gitcmd.In(cwd, "log", "-1", "--format=%h %s").Output(); err == nil {
			info.lastCommit = sanitizeLabel(strings.TrimSpace(string(out)))
		}
		return gitStatusMsg{info}
	}
}

func (g *gitInfo) counts() (staged, unstaged, untracked int) {
	for _, f := range g.files {
		if f.x == '?' {
			untracked++
			continue
		}
		if f.x != ' ' {
			staged++
		}
		if f.y != ' ' {
			unstaged++
		}
	}
	return
}

func renderGitPanel(m *App, a *agent.Agent, w, h int) []string {
	g := &m.git
	if !g.ok {
		return []string{styDim.Render("not a git repository")}
	}
	branch := styAccent.Render(truncate(g.branch, w-12))
	sync := ""
	if g.ahead > 0 {
		sync += styOK.Render(fmt.Sprintf(" ^%d", g.ahead))
	}
	if g.behind > 0 {
		sync += styWarn.Render(fmt.Sprintf(" v%d", g.behind))
	}
	staged, unstaged, untracked := g.counts()
	lines := []string{
		fmt.Sprintf("%s %s%s", styDim.Render("branch"), branch, sync),
	}
	if g.lastCommit != "" {
		lines = append(lines, fmt.Sprintf("%s %s", styDim.Render("last"), truncate(g.lastCommit, w-7)))
	}
	lines = append(lines, fmt.Sprintf("%s %s"+gl.sep+"%s %s"+gl.sep+"%s %s",
		styOK.Render(humanInt(staged)), styDim.Render("staged"),
		styWarn.Render(humanInt(unstaged)), styDim.Render("unstaged"),
		styErr.Render(humanInt(untracked)), styDim.Render("untracked")))
	if len(g.files) == 0 {
		lines = append(lines, styDim.Render("working tree clean"))
	}
	lines = append(lines, styDim.Render("/diff shows the full diff"))
	return lines
}

func renderChangesPanel(m *App, a *agent.Agent, w, h int) []string {
	g := &m.git
	if !g.ok {
		return []string{styDim.Render("not a git repository")}
	}
	if len(g.files) == 0 {
		return []string{styDim.Render("working tree clean")}
	}
	var lines []string
	for _, f := range g.files {
		if len(lines) >= h {
			lines[h-1] = styDim.Render(fmt.Sprintf("+%d more…", len(g.files)-h+1))
			break
		}
		var status string
		if f.x == '?' {
			status = styDim.Render("??")
		} else {
			sx := " "
			if f.x != ' ' {
				sx = styOK.Render(string(f.x))
			}
			sy := " "
			if f.y != ' ' {
				sy = styWarn.Render(string(f.y))
			}
			status = sx + sy
		}
		lines = append(lines, status+" "+truncate(f.path, max(6, w-4)))
	}
	return lines
}
