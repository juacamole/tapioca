package ui

import (
	"encoding/base64"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/textarea"
	tea "github.com/charmbracelet/bubbletea"

	"tapioca/internal/agent"
	"tapioca/internal/checkpoint"
	"tapioca/internal/project"
	"tapioca/internal/provider"
	"tapioca/internal/session"
	"tapioca/internal/tools"
)

// slashCmd is one /command usable from the prompt.
type slashCmd struct {
	name string
	args string // argument hint for help/completion
	help string
	run  func(m *App, arg string) tea.Cmd
}

// slashCmds is the command registry (also shown in /help and completion).
// Assigned in init to break the static initialization cycle with the help
// content builder.
var slashCmds []slashCmd

func init() { slashCmds = buildSlashCmds() }

func buildSlashCmds() []slashCmd {
	return []slashCmd{
		{"help", "", "show keybinds and commands", func(m *App, _ string) tea.Cmd {
			m.openHelp()
			return nil
		}},
		{"settings", "", "edit the config file; reloads on save", func(m *App, _ string) tea.Cmd {
			m.saveCfg() // write current state so the file is fresh
			return openPathInEditorCmd(m.cfg.Editor, editTargetConfig, m.cfg.Path())
		}},
		{"panels", "", "choose and order dashboard panels", func(m *App, _ string) tea.Cmd {
			m.openPanelsPicker()
			return nil
		}},
		{"theme", "[name]", "switch color theme (no arg: picker)", func(m *App, arg string) tea.Cmd {
			if arg == "" {
				m.openThemePicker()
				return nil
			}
			return m.applyTheme(arg)
		}},
		{"glyphs", "[name]", "switch glyph set: unicode|ascii|nerd", func(m *App, arg string) tea.Cmd {
			if arg == "" {
				m.openGlyphsPicker()
				return nil
			}
			return m.applyGlyphs(arg)
		}},
		{"systemprompt", "", "edit the system prompt in your editor", func(m *App, _ string) tea.Cmd {
			sys := ""
			if a := m.mgr.ActiveAgent(); a != nil {
				sys = a.SystemPrompt
			}
			return openEditorCmd(m.cfg.Editor, editTargetSystem, sys)
		}},
		{"model", "[provider:]name", "switch model (no arg: picker)", cmdModel},
		{"effort", "off|low|medium|high", "set thinking effort", cmdEffort},
		{"goal", "text | clear", "set a session goal for the agent", cmdGoal},
		{"remember", "fact | clear", "persist a project fact for the model", cmdRemember},
		{"btw", "note", "add context without asking for a reply", cmdBtw},
		{"cd", "dir", "change the working directory", cmdCd},
		{"diff", "", "show git diff of the working directory", cmdDiff},
		{"permissions", "", "show what runs without approval", cmdPermissions},
		{"checkpoints", "", "pick a checkpoint to rewind the working tree to", cmdCheckpoints},
		{"rewind", "[n|id]", "rewind file changes (default: latest checkpoint)", cmdRewind},
		{"clear", "", "clear this agent's conversation", cmdClear},
		{"compact", "", "summarize the conversation to free context", cmdCompact},
		{"regen", "", "regenerate the last response", cmdRegen},
		{"edit", "", "pull the last prompt back into the input", cmdEdit},
		{"fork", "", "fork this conversation into a new agent", cmdFork},
		{"agent", "[name]", "create a new agent", cmdAgent},
		{"resume", "[id]", "resume a saved session", cmdResume},
		{"new", "", "start a fresh session", func(m *App, _ string) tea.Cmd {
			m.newSession()
			return tea.Batch(waitAgent(m.mgr.ActiveAgent()), m.flashCmd())
		}},
		{"save", "", "save the session", func(m *App, _ string) tea.Cmd {
			m.saveSession(true)
			return m.flashCmd()
		}},
		{"zen", "", "toggle zen mode (hide keybind hints)", func(m *App, _ string) tea.Cmd {
			m.cfg.Zen = !m.cfg.Zen
			zenMode = m.cfg.Zen
			m.ta.Placeholder = inputPlaceholder()
			m.saveCfg()
			m.refreshChat(false)
			if zenMode {
				m.setFlash("zen — /zen to exit", false)
			} else {
				m.setFlash("zen mode off", false)
			}
			return m.flashCmd()
		}},
		{"verbose", "", "toggle verbose chat", func(m *App, _ string) tea.Cmd {
			m.cfg.Verbose = !m.cfg.Verbose
			m.saveCfg()
			m.refreshChat(false)
			m.setFlash("verbose chat "+onOff(m.cfg.Verbose), false)
			return m.flashCmd()
		}},
		{"thinking", "", "toggle thinking mode", func(m *App, _ string) tea.Cmd {
			if a := m.mgr.ActiveAgent(); a != nil {
				a.Thinking = !a.Thinking
				m.cfg.Thinking = a.Thinking
				m.saveCfg()
				m.setFlash("thinking "+onOff(a.Thinking), false)
			}
			return m.flashCmd()
		}},
		{"tools", "", "toggle tools for this agent", func(m *App, _ string) tea.Cmd {
			if a := m.mgr.ActiveAgent(); a != nil {
				a.ToolsEnabled = !a.ToolsEnabled
				m.setFlash("tools "+onOff(a.ToolsEnabled), false)
			}
			return m.flashCmd()
		}},
		{"quit", "", "save and exit", func(m *App, _ string) tea.Cmd {
			if m.cfg.Autosave && m.dirty {
				m.saveSession(false)
			}
			return tea.Quit
		}},
	}
}

// sessionNewID re-exports session id generation for app.go.
func sessionNewID() string { return session.NewID() }

func findSlash(name string) *slashCmd {
	for i := range slashCmds {
		if slashCmds[i].name == name {
			return &slashCmds[i]
		}
	}
	return nil
}

// slashMatches returns the commands matching the current input prefix, or nil
// when the input is not in command-completion state.
func (m *App) slashMatches() []*slashCmd {
	v := m.ta.Value()
	if !strings.HasPrefix(v, "/") || strings.ContainsAny(v, "\n ") {
		return nil
	}
	pre := strings.TrimPrefix(v, "/")
	var out []*slashCmd
	for i := range slashCmds {
		if strings.HasPrefix(slashCmds[i].name, pre) {
			out = append(out, &slashCmds[i])
		}
	}
	// User commands complete alongside the built-ins.
	for i := range m.userCmds {
		uc := m.userCmds[i]
		if !strings.HasPrefix(uc.name, pre) {
			continue
		}
		label := uc.desc
		if uc.project {
			label += " (project)"
		}
		out = append(out, &slashCmd{name: uc.name, args: "[args]", help: label})
	}
	return out
}

// runSlash parses and executes a typed /command, recording it in the chat
// history as a local note (never sent to the model).
func (m *App) runSlash(text string) tea.Cmd {
	name, arg, _ := strings.Cut(strings.TrimPrefix(text, "/"), " ")
	arg = strings.TrimSpace(arg)
	c := findSlash(name)
	if c == nil {
		if uc, ok := m.findUserCmd(name); ok {
			m.noteSlash(text)
			cmd := m.runUserCmd(uc, arg)
			m.refreshChat(true)
			return cmd
		}
		m.setFlash("unknown command /"+name+" — /help lists commands", true)
		return m.flashCmd()
	}
	m.noteSlash(text)
	cmd := c.run(m, arg)
	m.refreshChat(true)
	return cmd
}

// noteSlash records a typed command in the transcript as a local note; it is
// never sent to the model.
func (m *App) noteSlash(text string) {
	a := m.mgr.ActiveAgent()
	if a == nil {
		return
	}
	a.Messages = append(a.Messages, provider.Message{
		Role:   "note",
		Blocks: []provider.Block{{Type: "text", Text: strings.TrimSpace(text)}},
		Time:   time.Now(),
	})
	m.dirty = true
}

// busyGuard flashes and reports true when the active agent is mid-turn.
func (m *App) busyGuard(a *agent.Agent) bool {
	if a != nil && a.Status.Busy() {
		m.setFlash("agent is busy — ctrl+c stops it first", true)
		return true
	}
	return false
}

func cmdModel(m *App, arg string) tea.Cmd {
	if arg == "" {
		m.setFlash("loading models…", false)
		return tea.Batch(m.loadModelsCmd(), m.flashCmd())
	}
	a := m.mgr.ActiveAgent()
	if a == nil {
		return nil
	}
	provName, model := a.ProviderName, arg
	if p, rest, ok := strings.Cut(arg, ":"); ok {
		if _, exists := m.cfg.Providers[p]; exists && rest != "" {
			provName, model = p, rest
		}
	}
	p, err := m.mgr.ProviderFor(provName)
	if err != nil {
		m.setFlash(err.Error(), true)
		return m.flashCmd()
	}
	a.Provider = p
	a.ProviderName = provName
	a.ProviderErr = ""
	a.Model = model
	m.cfg.DefaultProvider = provName
	m.cfg.DefaultModel = model
	m.saveCfg()
	m.dirty = true
	m.setFlash(provName+" -> "+model, false)
	return m.flashCmd()
}

var effortLevels = map[string]int{"off": 0, "low": 1024, "medium": 4096, "high": 16384}

func effortName(a *agent.Agent) string {
	switch {
	case !a.Thinking:
		return "off"
	case a.ThinkingBudget <= 1024:
		return "low"
	case a.ThinkingBudget <= 4096:
		return "medium"
	default:
		return "high"
	}
}

func cmdEffort(m *App, arg string) tea.Cmd {
	a := m.mgr.ActiveAgent()
	if a == nil {
		return nil
	}
	if arg == "" {
		m.setFlash("effort is "+effortName(a)+" — /effort off|low|medium|high", false)
		return m.flashCmd()
	}
	budget, ok := effortLevels[strings.ToLower(arg)]
	if !ok {
		m.setFlash("usage: /effort off|low|medium|high", true)
		return m.flashCmd()
	}
	a.Thinking = budget > 0
	m.cfg.Thinking = a.Thinking
	if budget > 0 {
		a.ThinkingBudget = budget
		m.cfg.ThinkingBudget = budget
	}
	m.saveCfg()
	m.dirty = true
	m.setFlash("effort "+strings.ToLower(arg), false)
	return m.flashCmd()
}

func cmdGoal(m *App, arg string) tea.Cmd {
	a := m.mgr.ActiveAgent()
	if a == nil {
		return nil
	}
	switch arg {
	case "":
		if a.Goal == "" {
			m.setFlash("no goal set — /goal <text> to set one", false)
		} else {
			m.setFlash("goal: "+truncate(a.Goal, 80), false)
		}
	case "clear", "none":
		a.Goal = ""
		m.dirty = true
		m.setFlash("goal cleared", false)
	default:
		a.Goal = arg
		m.dirty = true
		m.setFlash("goal set", false)
	}
	return m.flashCmd()
}

func cmdRemember(m *App, arg string) tea.Cmd {
	cwd := m.cwd()
	switch arg {
	case "":
		mem := project.Memory(cwd)
		if mem == "" {
			m.setFlash("nothing remembered for this project — /remember <fact> adds", false)
			return m.flashCmd()
		}
		m.openTextOverlay("project memory — "+shortPath(cwd), mem+"\n\n"+
			styDim.Render("loaded into every system prompt here"+gl.sep+"/remember clear wipes"))
		return nil
	case "clear":
		if err := project.ClearMemory(cwd); err != nil {
			m.setFlash(err.Error(), true)
		} else {
			m.setFlash("project memory cleared", false)
		}
	default:
		if err := project.Remember(cwd, arg); err != nil {
			m.setFlash(err.Error(), true)
		} else {
			m.setFlash("remembered — the model sees it from the next prompt on", false)
		}
	}
	return m.flashCmd()
}

func cmdBtw(m *App, arg string) tea.Cmd {
	a := m.mgr.ActiveAgent()
	if a == nil || m.busyGuard(a) {
		return m.flashCmd()
	}
	if arg == "" {
		m.setFlash("usage: /btw <note for the model>", true)
		return m.flashCmd()
	}
	a.Messages = append(a.Messages, provider.TextMessage("user",
		"(side note, no reply needed) "+arg))
	m.dirty = true
	m.refreshChat(true)
	m.setFlash("noted — will be included with the next prompt", false)
	return m.flashCmd()
}

func cmdCd(m *App, arg string) tea.Cmd {
	e := m.mgr.Exec
	if e == nil {
		return nil
	}
	if arg == "" {
		m.setFlash("cwd: "+e.Cwd(), false)
		return m.flashCmd()
	}
	dir := arg
	if strings.HasPrefix(dir, "~") {
		if home, err := os.UserHomeDir(); err == nil {
			dir = filepath.Join(home, strings.TrimPrefix(strings.TrimPrefix(dir, "~"), "/"))
		}
	}
	if !filepath.IsAbs(dir) {
		dir = filepath.Join(e.Cwd(), dir)
	}
	if err := e.SetCwd(filepath.Clean(dir)); err != nil {
		m.setFlash(err.Error(), true)
		return m.flashCmd()
	}
	m.reloadUserCmds() // .tapioca/commands is per project
	note := "cwd: " + e.Cwd()
	if project.Instructions(e.Cwd()) != "" {
		note += "" + gl.sep + "loaded project instructions"
	}
	if n := len(m.userCmds); n > 0 {
		note += fmt.Sprintf("%s%d commands", gl.sep, n)
	}
	m.setFlash(note, false)
	return tea.Batch(m.flashCmd(), fetchGitCmd(e.Cwd()))
}

func cmdDiff(m *App, _ string) tea.Cmd {
	cwd := "."
	if m.mgr.Exec != nil {
		cwd = m.mgr.Exec.Cwd()
	}
	unstaged, _ := exec.Command("git", "-C", cwd, "diff").CombinedOutput()
	staged, _ := exec.Command("git", "-C", cwd, "diff", "--cached").CombinedOutput()
	var parts []string
	if len(staged) > 0 {
		parts = append(parts, styPanelTitle.Render("staged")+"\n"+colorizeDiff(string(staged)))
	}
	if len(unstaged) > 0 {
		parts = append(parts, styPanelTitle.Render("unstaged")+"\n"+colorizeDiff(string(unstaged)))
	}
	if len(parts) == 0 {
		m.setFlash("no changes (git diff is empty)", false)
		return m.flashCmd()
	}
	m.openTextOverlay("git diff — "+cwd, strings.Join(parts, "\n\n"))
	return nil
}

func colorizeDiff(s string) string {
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		switch {
		case strings.HasPrefix(l, "+++"), strings.HasPrefix(l, "---"), strings.HasPrefix(l, "diff "), strings.HasPrefix(l, "index "):
			lines[i] = styDim.Render(l)
		case strings.HasPrefix(l, "@@"):
			lines[i] = styAccent.Render(l)
		case strings.HasPrefix(l, "+"):
			lines[i] = styOK.Render(l)
		case strings.HasPrefix(l, "-"):
			lines[i] = styErr.Render(l)
		}
	}
	return strings.Join(lines, "\n")
}

// sandboxLine describes what bash can reach, including the case where the
// sandbox is configured but unusable.
func sandboxLine(m *App) string {
	if !m.cfg.Sandbox {
		return styDim.Render("off — bash runs with your full access")
	}
	if !tools.SandboxAvailable() {
		return styErr.Render("enabled but bwrap is missing — bash calls fail")
	}
	net := "network allowed"
	if !m.cfg.SandboxNetwork {
		net = "no network"
	}
	return styOK.Render("on") + styDim.Render(" — bash sees the worktree writable, $HOME hidden, "+net)
}

func cmdPermissions(m *App, _ string) tea.Cmd {
	var b strings.Builder
	mode := m.permMode()
	b.WriteString(styPanelTitle.Render("mode") + "  " + styAccent.Render(mode) + styDim.Render("  ("+modeDescriptions[mode]+")") + "\n")
	b.WriteString(styPanelTitle.Render("sandbox") + "  " + sandboxLine(m) + "\n\n")

	b.WriteString(styPanelTitle.Render("never prompts") + "\n")
	b.WriteString("  read_file, grep, glob, web_search, web_fetch " + styDim.Render("(read-only builtins)") + "\n")
	b.WriteString("  todo_write " + styDim.Render("(writes the plan panel, not the disk)") + "\n")
	if n := len(m.mgr.MCP.AllTools()); n > 0 {
		b.WriteString(fmt.Sprintf("  %d MCP tools prompt like builtins %s\n", n, styDim.Render("(grants show as mcp:name)")))
	}
	switch mode {
	case "auto":
		b.WriteString("  write_file, edit_file " + styDim.Render("(auto mode)") + "\n")
	case "bypass":
		b.WriteString("  every tool " + styDim.Render("(bypass mode)") + "\n")
	}

	if m.mgr.Exec != nil {
		grantedTools, bashWords := m.mgr.Exec.Grants()
		var toolGrants, pathGrants, hostGrants []string
		for _, t := range grantedTools {
			switch {
			case strings.HasPrefix(t, "read:"):
				pathGrants = append(pathGrants, strings.TrimPrefix(t, "read:"))
			case strings.HasPrefix(t, "fetch:"):
				hostGrants = append(hostGrants, strings.TrimPrefix(t, "fetch:"))
			default:
				toolGrants = append(toolGrants, t)
			}
		}
		b.WriteString("\n" + styPanelTitle.Render("session grants") + styDim.Render("  (reset on restart, [a] adds)") + "\n")
		if len(toolGrants) == 0 && len(pathGrants) == 0 && len(hostGrants) == 0 {
			b.WriteString(styDim.Render("  none") + "\n")
		}
		for _, t := range toolGrants {
			b.WriteString("  " + t + "\n")
		}
		for _, p := range pathGrants {
			b.WriteString("  read " + p + "\n")
		}
		for _, h := range hostGrants {
			b.WriteString("  fetch " + h + " " + styDim.Render("(any path on this host)") + "\n")
		}
		b.WriteString("\n" + styPanelTitle.Render("bash words") + styDim.Render("  (bash_allow in config, [p] adds)") + "\n")
		if len(bashWords) == 0 {
			b.WriteString(styDim.Render("  none") + "\n")
		}
		for _, w := range bashWords {
			b.WriteString("  " + w + " *\n")
		}
	}
	// Rules outrank both the mode and the session grants above, so a page
	// about what runs without approval is wrong without them.
	p := m.cfg.Permissions
	if len(p.Allow)+len(p.Ask)+len(p.Deny) > 0 {
		b.WriteString("\n" + styPanelTitle.Render("rules") + styDim.Render("  ([permissions] in config)") + "\n")
		for _, r := range p.Deny {
			b.WriteString("  " + styErr.Render("deny  ") + r + "\n")
		}
		for _, r := range p.Ask {
			b.WriteString("  " + styWarn.Render("ask   ") + r + "\n")
		}
		for _, r := range p.Allow {
			b.WriteString("  " + styOK.Render("allow ") + r + "\n")
		}
	}
	b.WriteString("\n" + styDim.Render("revoke: /settings edits bash_allow and [permissions]; session grants clear on restart"))
	m.openTextOverlay("permissions", b.String())
	return nil
}

func cmdCheckpoints(m *App, _ string) tea.Cmd {
	if m.busyGuard(m.mgr.ActiveAgent()) {
		return m.flashCmd()
	}
	entries, err := checkpoint.List(m.cwd(), 30)
	if err != nil || len(entries) == 0 {
		m.setFlash("no checkpoints yet — they appear once the agent modifies files", true)
		return m.flashCmd()
	}
	var items []pickerItem
	for _, e := range entries {
		items = append(items, pickerItem{
			label: e.Label,
			desc:  e.ID + "" + gl.sep + "" + ago(e.Time),
			value: e.ID,
		})
	}
	m.pick = newPicker(pickCheckpoint, "rewind working tree to…", items)
	m.overlay = overlayPicker
	return nil
}

func cmdRewind(m *App, arg string) tea.Cmd {
	if m.busyGuard(m.mgr.ActiveAgent()) {
		return m.flashCmd()
	}
	entries, err := checkpoint.List(m.cwd(), 30)
	if err != nil || len(entries) == 0 {
		m.setFlash("no checkpoints to rewind to", true)
		return m.flashCmd()
	}
	id := entries[0].ID
	if arg != "" {
		if n, err := strconv.Atoi(arg); err == nil && n >= 1 && n <= len(entries) {
			id = entries[n-1].ID
		} else {
			id = arg
		}
	}
	return m.restoreCheckpoint(id)
}

// restoreCheckpoint rewinds asynchronously; the current state is snapshotted
// first so the rewind itself can be undone.
func (m *App) restoreCheckpoint(id string) tea.Cmd {
	cwd := m.cwd()
	m.setFlash("rewinding to "+id+"…", false)
	return tea.Batch(m.flashCmd(), func() tea.Msg {
		return rewindDoneMsg{id: id, err: checkpoint.Restore(cwd, id)}
	})
}

func cmdClear(m *App, _ string) tea.Cmd {
	a := m.mgr.ActiveAgent()
	if a == nil || m.busyGuard(a) {
		return m.flashCmd()
	}
	a.Messages = nil
	a.Queue = nil
	a.StreamText = ""
	a.StreamThinking = ""
	a.LastErr = ""
	a.CtxTokens = 0
	m.dirty = true
	m.refreshChat(true)
	m.setFlash("conversation cleared", false)
	return m.flashCmd()
}

func cmdCompact(m *App, _ string) tea.Cmd {
	a := m.mgr.ActiveAgent()
	if a == nil || m.busyGuard(a) {
		return m.flashCmd()
	}
	cmd := m.compactCmd(a)
	if cmd == nil {
		m.setFlash("nothing to compact yet", true)
		return m.flashCmd()
	}
	a.Status = agent.StatusWaiting
	a.StatusDetail = "compacting conversation"
	m.setFlash("compacting conversation…", false)
	return tea.Batch(cmd, m.flashCmd())
}

// lastUserIdx finds the newest real user prompt (not tool results or hidden
// notices).
func lastUserIdx(a *agent.Agent) int {
	for i := len(a.Messages) - 1; i >= 0; i-- {
		if a.Messages[i].Role == "user" && !a.Messages[i].IsToolResult() && !a.Messages[i].Hidden {
			return i
		}
	}
	return -1
}

func cmdRegen(m *App, _ string) tea.Cmd {
	a := m.mgr.ActiveAgent()
	if a == nil || m.busyGuard(a) {
		return m.flashCmd()
	}
	i := lastUserIdx(a)
	if i < 0 {
		m.setFlash("nothing to regenerate", true)
		return m.flashCmd()
	}
	a.Messages = a.Messages[:i+1]
	history := make([]provider.Message, len(a.Messages))
	copy(history, a.Messages)
	a.LastErr = ""
	a.Status = agent.StatusWaiting
	a.StatusDetail = "sending request"
	a.StreamStart = time.Now()
	a.Send(history)
	m.dirty = true
	m.refreshChat(true)
	return nil
}

func cmdEdit(m *App, _ string) tea.Cmd {
	a := m.mgr.ActiveAgent()
	if a == nil || m.busyGuard(a) {
		return m.flashCmd()
	}
	i := lastUserIdx(a)
	if i < 0 {
		m.setFlash("no prompt to edit", true)
		return m.flashCmd()
	}
	// Restore the raw typed prompt (not the mention-expanded text, which
	// would re-inline files on resend) and re-pend any image attachments.
	edited := a.Messages[i]
	text := edited.Typed
	if text == "" {
		text = edited.Text()
	}
	for _, bl := range edited.Blocks {
		if bl.Type == "image" {
			if data, err := base64.StdEncoding.DecodeString(bl.Data); err == nil {
				m.pending = append(m.pending, attachment{label: "(from edit)", mediaType: bl.MediaType, data: data})
			}
		}
	}
	a.Messages = a.Messages[:i]
	m.ta.SetValue(text)
	m.focus = focusInput
	m.recalcLayout()
	m.dirty = true
	m.refreshChat(true)
	return tea.Batch(m.ta.Focus(), textarea.Blink)
}

func cmdFork(m *App, _ string) tea.Cmd {
	a := m.mgr.ActiveAgent()
	if a == nil {
		return nil
	}
	na := m.mgr.Fork(a)
	m.mgr.Active = len(m.mgr.Agents) - 1
	m.dirty = true
	m.refreshChat(true)
	m.setFlash(fmt.Sprintf("forked %s -> %s", a.Name, na.Name), false)
	return tea.Batch(waitAgent(na), m.flashCmd())
}

func cmdAgent(m *App, arg string) tea.Cmd {
	na := m.mgr.NewAgent()
	if arg != "" {
		na.Name = arg
	}
	m.mgr.Active = len(m.mgr.Agents) - 1
	m.refreshChat(true)
	m.setFlash("created "+na.Name, false)
	return tea.Batch(waitAgent(na), m.flashCmd())
}

func cmdResume(m *App, arg string) tea.Cmd {
	if arg != "" {
		return m.loadSessionByID(arg)
	}
	return m.openSessionPicker()
}

// loadSessionByID replaces the workspace with a stored session.
func (m *App) loadSessionByID(id string) tea.Cmd {
	s, err := session.Load(id)
	if err != nil {
		m.setFlash(err.Error(), true)
		return m.flashCmd()
	}
	m.releaseAllSpawns("the session was replaced")
	m.mgr.LoadSession(s)
	m.sessID = s.ID
	m.sessName = s.Name
	m.sessTitled = s.Name != ""
	m.sessCreated = s.CreatedAt
	m.sessSaved = s.UpdatedAt
	m.dirty = false
	m.refreshChat(true)
	m.setFlash("resumed "+s.ID, false)
	cmds := []tea.Cmd{m.flashCmd()}
	for _, a := range m.mgr.Agents {
		cmds = append(cmds, waitAgent(a))
	}
	return tea.Batch(cmds...)
}

// openSessionPicker lists sessions; typing in the picker searches names AND
// full conversation text.
func (m *App) openSessionPicker() tea.Cmd {
	metas, err := session.ListWithText()
	if err != nil {
		m.setFlash("listing sessions: "+err.Error(), true)
		return m.flashCmd()
	}
	if len(metas) == 0 {
		m.setFlash("no saved sessions yet", true)
		return m.flashCmd()
	}
	// This project's sessions first: resuming almost always means "what I was
	// doing here", and a flat list of every session ever is unreadable.
	here, elsewhere := session.ForProject(metas, m.cwd())
	item := func(meta session.Meta, other bool) pickerItem {
		label := meta.Name
		if label == "" {
			label = meta.ID
		}
		desc := fmt.Sprintf("%d agents"+gl.sep+"%d msgs"+gl.sep+"%s", meta.Agents, meta.Messages, ago(meta.UpdatedAt))
		if other {
			// Sessions saved before directories were recorded have none.
			where := "another project"
			if meta.Cwd != "" {
				where = shortPath(meta.Cwd)
			}
			desc += gl.sep + where
			label = styDim.Render(label)
		}
		return pickerItem{label: label, desc: desc, value: meta.ID, search: meta.Blob}
	}
	var items []pickerItem
	for _, meta := range here {
		items = append(items, item(meta, false))
	}
	for _, meta := range elsewhere {
		items = append(items, item(meta, true))
	}
	title := "resume session (type to search all messages)"
	if len(here) > 0 && len(elsewhere) > 0 {
		title = fmt.Sprintf("resume session%s%d here, %d elsewhere", gl.sep, len(here), len(elsewhere))
	}
	m.pick = newPicker(pickSession, title, items)
	m.overlay = overlayPicker
	return nil
}
