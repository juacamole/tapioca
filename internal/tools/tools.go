// Package tools implements Tapioca's built-in coding tools (shell, file
// read/write/edit) with a permission gate, making agents able to work on a
// codebase without any MCP server.
package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"

	"tapioca/internal/provider"
)

// Decision is the user's answer to a permission request.
type Decision struct {
	Allow  bool
	Always bool // remember for this tool for the rest of the session
}

// AskFunc asks the user for permission; it blocks until answered.
type AskFunc func(tool, summary string) Decision

// Permission modes, cycled with shift+tab in the UI.
const (
	ModePlan   = "plan"   // no file modifications; bash asks; agent presents plans
	ModeManual = "manual" // ask for every mutating tool call
	ModeAuto   = "auto"   // file edits auto-approved; bash still asks
	ModeBypass = "bypass" // everything runs without asking
)

// Modes lists the cycle order.
var Modes = []string{ModePlan, ModeManual, ModeAuto, ModeBypass}

// NormalizeMode maps legacy/unknown mode names onto the current set.
func NormalizeMode(m string) string {
	switch m {
	case ModePlan, "readonly":
		return ModePlan
	case ModeAuto:
		return ModeAuto
	case ModeBypass:
		return ModeBypass
	default: // "", "ask", "manual", anything else
		return ModeManual
	}
}

const maxOutput = 30_000

// Executor runs built-in tools inside a working directory.
type Executor struct {
	mu      sync.Mutex
	cwd     string
	mode    string
	allowed map[string]bool
}

// NewExecutor creates an executor rooted at cwd.
func NewExecutor(cwd, mode string) *Executor {
	return &Executor{cwd: cwd, mode: NormalizeMode(mode), allowed: map[string]bool{}}
}

// SetMode switches the permission mode.
func (e *Executor) SetMode(mode string) {
	e.mu.Lock()
	e.mode = NormalizeMode(mode)
	e.mu.Unlock()
}

// Cwd returns the current working directory.
func (e *Executor) Cwd() string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.cwd
}

// SetCwd changes the working directory; the path must exist.
func (e *Executor) SetCwd(dir string) error {
	info, err := os.Stat(dir)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("%s is not a directory", dir)
	}
	e.mu.Lock()
	e.cwd = dir
	e.mu.Unlock()
	return nil
}

// Mode returns the permission mode.
func (e *Executor) Mode() string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.mode
}

var toolNames = []string{"bash", "read_file", "write_file", "edit_file", "web_search", "web_fetch"}

// Has reports whether name is a built-in tool.
func (e *Executor) Has(name string) bool {
	for _, t := range toolNames {
		if t == name {
			return true
		}
	}
	return false
}

// Tools returns the tool definitions offered to models.
func (e *Executor) Tools() []provider.ToolDef {
	return []provider.ToolDef{
		{
			Name:        "bash",
			Description: "Run a shell command in the working directory and return its combined output. Use for builds, tests, git, search, etc.",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"command":{"type":"string","description":"The shell command to run"}},"required":["command"]}`),
		},
		{
			Name:        "read_file",
			Description: "Read a file (absolute path or relative to the working directory). Returns the file content.",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"},"offset":{"type":"integer","description":"1-based line to start from"},"limit":{"type":"integer","description":"max lines to return"}},"required":["path"]}`),
		},
		{
			Name:        "write_file",
			Description: "Create or overwrite a file with the given content. Parent directories are created.",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"},"content":{"type":"string"}},"required":["path","content"]}`),
		},
		{
			Name:        "edit_file",
			Description: "Replace an exact string in a file. old_string must match exactly and be unique unless replace_all is true.",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"},"old_string":{"type":"string"},"new_string":{"type":"string"},"replace_all":{"type":"boolean"}},"required":["path","old_string","new_string"]}`),
		},
		{
			Name:        "web_search",
			Description: "Search the web. Returns result titles, URLs and snippets. Use web_fetch to read a result.",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"query":{"type":"string"},"max_results":{"type":"integer","description":"1-10, default 5"}},"required":["query"]}`),
		},
		{
			Name:        "web_fetch",
			Description: "Fetch a URL and return its readable text content.",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"url":{"type":"string"}},"required":["url"]}`),
		},
	}
}

func (e *Executor) resolve(path string) string {
	if strings.HasPrefix(path, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			path = filepath.Join(home, path[2:])
		}
	}
	if !filepath.IsAbs(path) {
		path = filepath.Join(e.Cwd(), path)
	}
	return filepath.Clean(path)
}

// Call executes a built-in tool. It returns (result, isError, err); err is
// reserved for internal failures, tool-level problems are (msg, true, nil).
func (e *Executor) Call(ctx context.Context, name string, raw json.RawMessage, ask AskFunc) (string, bool, error) {
	readOnly := name == "read_file" || name == "web_search" || name == "web_fetch"
	if !readOnly { // mutating tools go through the permission gate
		mode := e.Mode()
		fileEdit := name == "write_file" || name == "edit_file"
		needAsk := false
		switch mode {
		case ModePlan:
			if fileEdit {
				return "denied: plan mode is active — do not modify files; present a plan instead", true, nil
			}
			needAsk = true // bash still allowed for read-only inspection, but asks
		case ModeManual:
			needAsk = true
		case ModeAuto:
			needAsk = !fileEdit // edits auto-approved, bash asks
		case ModeBypass:
			needAsk = false
		}
		if needAsk {
			e.mu.Lock()
			ok := e.allowed[name]
			e.mu.Unlock()
			if !ok {
				d := ask(name, e.summary(name, raw))
				if !d.Allow {
					return "the user denied permission for this call", true, nil
				}
				if d.Always {
					e.mu.Lock()
					e.allowed[name] = true
					e.mu.Unlock()
				}
			}
		}
	}

	switch name {
	case "bash":
		return e.runBash(ctx, raw)
	case "read_file":
		return e.readFile(raw)
	case "write_file":
		return e.writeFile(raw)
	case "edit_file":
		return e.editFile(raw)
	case "web_search":
		return e.webSearch(ctx, raw)
	case "web_fetch":
		return e.webFetch(ctx, raw)
	}
	return "", true, fmt.Errorf("unknown builtin tool %q", name)
}

// summary produces the one-line description shown in the permission prompt.
func (e *Executor) summary(name string, raw json.RawMessage) string {
	switch name {
	case "bash":
		var a struct {
			Command string `json:"command"`
		}
		_ = json.Unmarshal(raw, &a)
		return a.Command
	default:
		var a struct {
			Path string `json:"path"`
		}
		_ = json.Unmarshal(raw, &a)
		return a.Path
	}
}

func capOutput(s string) string {
	if len(s) <= maxOutput {
		return s
	}
	return s[:maxOutput*2/3] + "\n[... output truncated ...]\n" + s[len(s)-maxOutput/3:]
}

func (e *Executor) runBash(ctx context.Context, raw json.RawMessage) (string, bool, error) {
	var a struct {
		Command string `json:"command"`
	}
	if err := json.Unmarshal(raw, &a); err != nil || strings.TrimSpace(a.Command) == "" {
		return "invalid arguments: need {\"command\": \"...\"}", true, nil
	}
	cmd := exec.CommandContext(ctx, "sh", "-c", a.Command)
	cmd.Dir = e.Cwd()
	out, err := cmd.CombinedOutput()
	text := capOutput(string(out))
	if err != nil {
		if text != "" {
			text += "\n"
		}
		text += fmt.Sprintf("(%v)", err)
		return text, true, nil
	}
	if strings.TrimSpace(text) == "" {
		text = "(no output)"
	}
	return text, false, nil
}

func (e *Executor) readFile(raw json.RawMessage) (string, bool, error) {
	var a struct {
		Path   string `json:"path"`
		Offset int    `json:"offset"`
		Limit  int    `json:"limit"`
	}
	if err := json.Unmarshal(raw, &a); err != nil || a.Path == "" {
		return "invalid arguments: need {\"path\": \"...\"}", true, nil
	}
	data, err := os.ReadFile(e.resolve(a.Path))
	if err != nil {
		return err.Error(), true, nil
	}
	lines := strings.Split(string(data), "\n")
	start := 0
	if a.Offset > 1 {
		start = a.Offset - 1
	}
	if start >= len(lines) {
		return fmt.Sprintf("offset %d is past the end of the file (%d lines)", a.Offset, len(lines)), true, nil
	}
	end := len(lines)
	if a.Limit > 0 && start+a.Limit < end {
		end = start + a.Limit
	}
	out := strings.Join(lines[start:end], "\n")
	if end < len(lines) {
		out += fmt.Sprintf("\n[... %d more lines ...]", len(lines)-end)
	}
	return capOutput(out), false, nil
}

func (e *Executor) writeFile(raw json.RawMessage) (string, bool, error) {
	var a struct {
		Path    string `json:"path"`
		Content string `json:"content"`
	}
	if err := json.Unmarshal(raw, &a); err != nil || a.Path == "" {
		return "invalid arguments: need {\"path\", \"content\"}", true, nil
	}
	path := e.resolve(a.Path)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err.Error(), true, nil
	}
	if err := os.WriteFile(path, []byte(a.Content), 0o644); err != nil {
		return err.Error(), true, nil
	}
	return fmt.Sprintf("wrote %d bytes to %s", len(a.Content), path), false, nil
}

func (e *Executor) editFile(raw json.RawMessage) (string, bool, error) {
	var a struct {
		Path       string `json:"path"`
		OldString  string `json:"old_string"`
		NewString  string `json:"new_string"`
		ReplaceAll bool   `json:"replace_all"`
	}
	if err := json.Unmarshal(raw, &a); err != nil || a.Path == "" || a.OldString == "" {
		return "invalid arguments: need {\"path\", \"old_string\", \"new_string\"}", true, nil
	}
	path := e.resolve(a.Path)
	data, err := os.ReadFile(path)
	if err != nil {
		return err.Error(), true, nil
	}
	content := string(data)
	count := strings.Count(content, a.OldString)
	switch {
	case count == 0:
		return "old_string not found in file", true, nil
	case count > 1 && !a.ReplaceAll:
		return fmt.Sprintf("old_string appears %d times; make it unique or set replace_all", count), true, nil
	}
	if a.ReplaceAll {
		content = strings.ReplaceAll(content, a.OldString, a.NewString)
	} else {
		content = strings.Replace(content, a.OldString, a.NewString, 1)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		return err.Error(), true, nil
	}
	return fmt.Sprintf("edited %s (%d replacement(s))", path, count), false, nil
}
