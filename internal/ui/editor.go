package ui

import (
	"errors"
	"os"
	"os/exec"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
)

// Editor targets.
const (
	editTargetPrompt = iota
	editTargetSystem
	editTargetConfig
)

type editorDoneMsg struct {
	target  int
	content string
	err     error
}

// resolveEditor picks the editor command: config, $VISUAL, $EDITOR, then
// nvim/vim/vi. The returned slice is argv (the config value may hold args).
func resolveEditor(cfgEditor string) ([]string, error) {
	candidates := []string{cfgEditor, os.Getenv("VISUAL"), os.Getenv("EDITOR"), "nvim", "vim", "vi"}
	for _, c := range candidates {
		c = strings.TrimSpace(c)
		if c == "" {
			continue
		}
		parts := strings.Fields(c)
		if _, err := exec.LookPath(parts[0]); err == nil {
			return parts, nil
		}
	}
	return nil, errors.New("no editor found — set editor in config or $EDITOR")
}

// openEditorCmd writes initial content to a temp file, suspends the TUI and
// opens the editor on it, then reports the edited content back.
func openEditorCmd(cfgEditor string, target int, initial string) tea.Cmd {
	argv, err := resolveEditor(cfgEditor)
	if err != nil {
		return func() tea.Msg { return editorDoneMsg{target: target, err: err} }
	}
	f, err := os.CreateTemp("", "tapioca-*.md")
	if err != nil {
		// $TMPDIR may point somewhere stale; fall back to /tmp.
		f, err = os.CreateTemp("/tmp", "tapioca-*.md")
	}
	if err != nil {
		return func() tea.Msg { return editorDoneMsg{target: target, err: err} }
	}
	path := f.Name()
	if _, err := f.WriteString(initial); err != nil {
		f.Close()
		os.Remove(path)
		return func() tea.Msg { return editorDoneMsg{target: target, err: err} }
	}
	f.Close()

	c := exec.Command(argv[0], append(argv[1:], path)...)
	return tea.ExecProcess(c, func(procErr error) tea.Msg {
		data, readErr := os.ReadFile(path)
		os.Remove(path)
		err := procErr
		if err == nil {
			err = readErr
		}
		return editorDoneMsg{target: target, content: string(data), err: err}
	})
}

// openPathInEditorCmd edits an existing file in place (used by /settings for
// the config file).
func openPathInEditorCmd(cfgEditor string, target int, path string) tea.Cmd {
	argv, err := resolveEditor(cfgEditor)
	if err != nil {
		return func() tea.Msg { return editorDoneMsg{target: target, err: err} }
	}
	c := exec.Command(argv[0], append(argv[1:], path)...)
	return tea.ExecProcess(c, func(procErr error) tea.Msg {
		return editorDoneMsg{target: target, err: procErr}
	})
}
