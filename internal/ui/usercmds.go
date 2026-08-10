package ui

import (
	"os"
	"path/filepath"
	"sort"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"tapioca/internal/config"
	"tapioca/internal/provider"
	"tapioca/internal/textenc"
)

// Custom commands are markdown files: the body is a prompt, the filename is
// the command. Anything you retype often becomes a command without touching
// the binary. Project files sit in .tapioca/commands and override the personal
// ones in the config directory.

const (
	userCmdDir     = "commands"
	projectCmdDir  = ".tapioca"
	maxUserCmdSize = 64 * 1024
	argsToken      = "$ARGUMENTS"
)

type userCmd struct {
	name    string
	desc    string
	body    string
	project bool // from the worktree rather than the config directory
}

// loadUserCmds reads both command directories, project last so it wins.
func loadUserCmds(cwd string) []userCmd {
	byName := map[string]userCmd{}
	read := func(dir string, project bool) {
		entries, err := os.ReadDir(dir)
		if err != nil {
			return
		}
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(strings.ToLower(e.Name()), ".md") {
				continue
			}
			c, ok := parseUserCmd(filepath.Join(dir, e.Name()))
			if !ok {
				continue
			}
			c.project = project
			byName[c.name] = c
		}
	}
	read(filepath.Join(config.Dir(), userCmdDir), false)
	read(filepath.Join(cwd, projectCmdDir, userCmdDir), true)

	out := make([]userCmd, 0, len(byName))
	for _, c := range byName {
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].name < out[j].name })
	return out
}

// parseUserCmd reads one command file. A leading "# heading" line becomes the
// description shown in completion, since a bare filename says little.
func parseUserCmd(path string) (userCmd, bool) {
	data, err := os.ReadFile(path)
	if err != nil || len(data) > maxUserCmdSize {
		return userCmd{}, false
	}
	text, ok := textenc.Decode(data)
	if !ok || strings.TrimSpace(text) == "" {
		return userCmd{}, false
	}
	name := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	name = strings.ToLower(strings.TrimSpace(name))
	if name == "" || strings.ContainsAny(name, " \t/") {
		return userCmd{}, false
	}

	body := strings.TrimSpace(text)
	desc := ""
	if lines := strings.SplitN(body, "\n", 2); strings.HasPrefix(lines[0], "#") {
		desc = strings.TrimSpace(strings.TrimLeft(lines[0], "# "))
		if len(lines) > 1 {
			body = strings.TrimSpace(lines[1])
		}
	}
	if desc == "" {
		desc = "custom command"
	}
	return userCmd{name: name, desc: desc, body: body}, true
}

// render substitutes the arguments. A template with no $ARGUMENTS still gets
// them, appended, so a command written without the token is not silently
// ignoring what was typed.
func (c userCmd) render(args string) string {
	args = strings.TrimSpace(args)
	if strings.Contains(c.body, argsToken) {
		return strings.ReplaceAll(c.body, argsToken, args)
	}
	if args == "" {
		return c.body
	}
	return c.body + "\n\n" + args
}

// findUserCmd looks up a loaded command by name.
func (m *App) findUserCmd(name string) (userCmd, bool) {
	for _, c := range m.userCmds {
		if c.name == name {
			return c, true
		}
	}
	return userCmd{}, false
}

// runUserCmd sends the rendered template as if it had been typed.
func (m *App) runUserCmd(c userCmd, args string) tea.Cmd {
	a := m.mgr.ActiveAgent()
	if a == nil {
		return nil
	}
	text := c.render(args)
	if strings.TrimSpace(text) == "" {
		m.setFlash("/"+c.name+" is empty", true)
		return m.flashCmd()
	}
	msg := provider.TextMessage("user", text)
	msg.Typed = "/" + c.name
	if args != "" {
		msg.Typed += " " + args
	}
	return m.sendPrepared(a, msg)
}

// reloadUserCmds picks up new files after /cd or a config edit.
func (m *App) reloadUserCmds() { m.userCmds = loadUserCmds(m.cwd()) }
