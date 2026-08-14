package ui

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

	"tapioca/internal/mcp"
	"tapioca/internal/provider"
)

// A server's prompts are templates a person invokes deliberately, which is
// what a slash command already is, and its resources are documents to put in
// front of the model, which is what @ already does. So they arrive through
// those doors rather than a new one: /<server>:<prompt> and @<server>:<uri>.

const mcpCallTimeout = 30 * time.Second

// mcpPromptCmds returns the connected servers' prompts as slash commands.
func (m *App) mcpPromptCmds() []slashCmd {
	if m.mgr.MCP == nil {
		return nil
	}
	prompts := m.mgr.MCP.AllPrompts()
	out := make([]slashCmd, 0, len(prompts))
	for _, p := range prompts {
		p := p
		desc := sanitizeLabel(p.Description)
		if desc == "" {
			desc = "prompt from " + sanitizeLabel(p.Server)
		}
		args := ""
		for _, a := range p.Arguments {
			name := "[" + a.Name + "]"
			if a.Required {
				name = "<" + a.Name + ">"
			}
			args += name + " "
		}
		out = append(out, slashCmd{
			name: sanitizeLabel(mcp.FullName(p.Server, p.Name)),
			args: strings.TrimSpace(args),
			help: desc,
			run:  func(m *App, arg string) tea.Cmd { return m.runMCPPrompt(p, arg) },
		})
	}
	return out
}

// findMCPPrompt looks up a prompt command by its namespaced name.
func (m *App) findMCPPrompt(name string) (slashCmd, bool) {
	for _, c := range m.mcpPromptCmds() {
		if c.name == name {
			return c, true
		}
	}
	return slashCmd{}, false
}

// promptArgs maps what was typed onto the prompt's declared arguments: one
// argument takes the whole line, several split on whitespace with the last
// taking the remainder, so a trailing message argument keeps its spaces.
func promptArgs(p mcp.Prompt, typed string) map[string]string {
	typed = strings.TrimSpace(typed)
	if len(p.Arguments) == 0 || typed == "" {
		return nil
	}
	out := map[string]string{}
	if len(p.Arguments) == 1 {
		out[p.Arguments[0].Name] = typed
		return out
	}
	fields := strings.SplitN(typed, " ", len(p.Arguments))
	for i, f := range fields {
		out[p.Arguments[i].Name] = strings.TrimSpace(f)
	}
	return out
}

// runMCPPrompt fetches the rendered prompt and sends it as if it had been
// typed. The round trip happens off the update loop.
func (m *App) runMCPPrompt(p mcp.Prompt, arg string) tea.Cmd {
	reg := m.mgr.MCP
	if reg == nil {
		return nil
	}
	args := promptArgs(p, arg)
	typed := "/" + mcp.FullName(p.Server, p.Name)
	if arg != "" {
		typed += " " + arg
	}
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), mcpCallTimeout)
		defer cancel()
		text, err := reg.GetPrompt(ctx, mcp.Sanitize(p.Server), p.Name, args)
		return mcpPromptMsg{typed: typed, text: text, err: err}
	}
}

// mcpPromptMsg carries a rendered prompt back to the update loop.
type mcpPromptMsg struct {
	typed string
	text  string
	err   error
}

func (m *App) handleMCPPrompt(msg mcpPromptMsg) tea.Cmd {
	if msg.err != nil {
		m.setFlash("prompt failed: "+msg.err.Error(), true)
		return m.flashCmd()
	}
	a := m.mgr.ActiveAgent()
	if a == nil {
		return nil
	}
	pm := provider.TextMessage("user", msg.text)
	pm.Typed = msg.typed
	cmd := m.sendPrepared(a, pm)
	m.refreshChat(true)
	return cmd
}

// mcpResourceMatches returns resource mentions matching the typed prefix, as
// "server:uri" so one server's uris cannot shadow another's.
func (m *App) mcpResourceMatches(tok string) []string {
	if m.mgr.MCP == nil {
		return nil
	}
	var out []string
	for _, r := range m.mgr.MCP.AllResources() {
		label := sanitizeLabel(mcp.Sanitize(r.Server) + ":" + r.URI)
		if tok == "" || strings.Contains(strings.ToLower(label), strings.ToLower(tok)) {
			out = append(out, label)
		}
	}
	sort.Strings(out)
	return out
}

// readMCPResource resolves a "server:uri" mention. It returns ok=false when the
// token is not one, so an ordinary @path is left alone.
func (m *App) readMCPResource(token string) (string, bool) {
	if m.mgr.MCP == nil {
		return "", false
	}
	server, uri, found := strings.Cut(token, ":")
	if !found || uri == "" {
		return "", false
	}
	// A URI has a scheme of its own (file://, db://), so "s:file://x" splits
	// into the server and the rest; a bare path like "./a:b" does not name a
	// known server and falls through to the file handling.
	known := false
	for _, r := range m.mgr.MCP.AllResources() {
		if mcp.Sanitize(r.Server) == server {
			known = true
			break
		}
	}
	if !known {
		return "", false
	}
	ctx, cancel := context.WithTimeout(context.Background(), mcpCallTimeout)
	defer cancel()
	text, err := m.mgr.MCP.ReadResource(ctx, server, uri)
	if err != nil {
		return fmt.Sprintf("[resource: %s — %v]", token, err), true
	}
	return fmt.Sprintf("[resource: %s]\n```\n%s\n```", token, text), true
}
