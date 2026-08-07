package ui

import (
	"context"
	"fmt"
	"math"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/textarea"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/reflow/wrap"

	"tapioca/internal/agent"
	"tapioca/internal/catalog"
	"tapioca/internal/config"
	"tapioca/internal/mcp"
	"tapioca/internal/provider"
	"tapioca/internal/tools"
)

type focusArea int

const (
	focusInput focusArea = iota
	focusChat
	focusDash
)

type overlayKind int

const (
	overlayNone overlayKind = iota
	overlayPicker
	overlayHelp
	overlayPerm
	overlayText
)

// quitWindow is how long the second ctrl+c has to arrive to exit.
const quitWindow = 2 * time.Second

// Messages.
type agentEventMsg struct{ ev agent.Event }
type mcpConnectedMsg struct {
	name  string
	tools int
	err   error
}
type modelsLoadedMsg struct {
	items []pickerItem
	errs  []string
}
type flashClearMsg struct{ seq int }
type autosaveMsg struct{}
type startPickerMsg struct{}
type rewindDoneMsg struct {
	id  string
	err error
}
type compactDoneMsg struct {
	agentID int
	msgs    []provider.Message
	ctxEst  int
	err     error
}

type permEntry struct {
	agentID int
	req     *agent.PermissionReq
}

// App is the root Bubble Tea model.
type App struct {
	cfg  *config.Config
	keys *KeyMap
	mgr  *agent.Manager

	w, h  int
	ready bool

	ta   textarea.Model
	vp   viewport.Model
	spin spinner.Model

	focus        focusArea
	dashPanelSel int  // focused panel when the dashboard has focus
	dashEditing  bool // inside the settings panel, editing rows
	dashSel      int  // selected settings row while editing

	overlay   overlayKind
	pick      picker
	perms     []permEntry
	textTitle string
	textVP    viewport.Model

	slashSel  int
	recalling bool
	recallIdx int

	mouseOn   bool
	git       gitInfo
	probedCtx map[string]bool

	// Chat transcript caches for mouse selection.
	chatStyled []string
	chatPlain  []string
	selActive  bool
	selStart   selPoint
	selEnd     selPoint

	sessID      string
	sessName    string
	sessCreated time.Time
	sessSaved   time.Time
	dirty       bool

	quitArmed     time.Time // last ctrl+c press while idle
	pickerOnStart bool

	flash    string
	flashErr bool
	flashSeq int
}

// zenMode hides every keybind hint; UI-thread only, mirrored from cfg.Zen.
var zenMode bool

func inputPlaceholder() string {
	if zenMode {
		return "write a prompt…"
	}
	return "write a prompt… (enter sends, / for commands)"
}

// NewApp builds the root model. The manager must already hold at least one
// agent (fresh or restored from a session).
func NewApp(cfg *config.Config, mgr *agent.Manager, sessID, sessName string, created time.Time) *App {
	zenMode = cfg.Zen
	ta := textarea.New()
	ta.Placeholder = inputPlaceholder()
	ta.Prompt = "| "
	ta.CharLimit = 0
	ta.ShowLineNumbers = false
	ta.Focus()
	ta.KeyMap.InsertNewline.SetEnabled(false)

	sp := spinner.New(spinner.WithSpinner(spinner.MiniDot))
	sp.Style = styAccent

	return &App{
		cfg:         cfg,
		keys:        NewKeyMap(cfg.Keys),
		mgr:         mgr,
		ta:          ta,
		vp:          viewport.New(0, 0),
		spin:        sp,
		mouseOn:     true,
		probedCtx:   map[string]bool{},
		sessID:      sessID,
		sessName:    sessName,
		sessCreated: created,
	}
}

// statusLabel returns the fine-grained stage label when one is active.
func statusLabel(a *agent.Agent) string {
	if a.Status.Busy() {
		if left := time.Until(a.RetryAt); left > 0 {
			return fmt.Sprintf("retry in %ds (%s)", int(left.Seconds())+1, a.RetryNote)
		}
		if a.StatusDetail != "" {
			return a.StatusDetail
		}
	}
	return a.Status.String()
}

// StartWithSessionPicker opens the resume picker on launch (tapioca -r).
func (m *App) StartWithSessionPicker() { m.pickerOnStart = true }

// cwd returns the executor working directory.
func (m *App) cwd() string {
	if m.mgr.Exec != nil {
		return m.mgr.Exec.Cwd()
	}
	return "."
}

// Init implements tea.Model.
func (m *App) Init() tea.Cmd {
	cmds := []tea.Cmd{textarea.Blink, m.spin.Tick, autosaveTick(), gitTick(), fetchGitCmd(m.cwd())}
	for _, a := range m.mgr.Agents {
		cmds = append(cmds, waitAgent(a))
	}
	for _, sc := range m.cfg.MCP {
		cmds = append(cmds, connectMCPCmd(m.mgr.MCP, sc))
	}
	if m.pickerOnStart {
		cmds = append(cmds, func() tea.Msg { return startPickerMsg{} })
	}
	return tea.Batch(cmds...)
}

func waitAgent(a *agent.Agent) tea.Cmd {
	ch := a.Events
	return func() tea.Msg {
		ev, ok := <-ch
		if !ok {
			return nil
		}
		return agentEventMsg{ev}
	}
}

func connectMCPCmd(reg *mcp.Registry, sc config.MCPServerConfig) tea.Cmd {
	return func() tea.Msg {
		c, err := mcp.Start(context.Background(), sc)
		if err != nil {
			reg.SetError(sc.Name, err.Error())
			return mcpConnectedMsg{name: sc.Name, err: err}
		}
		reg.Add(c)
		return mcpConnectedMsg{name: sc.Name, tools: len(c.Tools)}
	}
}

func autosaveTick() tea.Cmd {
	return tea.Tick(30*time.Second, func(time.Time) tea.Msg { return autosaveMsg{} })
}

// saveCfg persists the config file; failures surface as a flash.
func (m *App) saveCfg() {
	if err := m.cfg.Save(); err != nil {
		m.setFlash(fmt.Sprintf("saving config: %v", err), true)
	}
}

// dismissOverlay closes the current overlay, surfacing any queued permission
// prompts instead of going straight back to the chat.
func (m *App) dismissOverlay() {
	if len(m.perms) > 0 {
		m.overlay = overlayPerm
	} else {
		m.overlay = overlayNone
	}
}

// openTextOverlay shows scrollable text (used by /diff).
func (m *App) openTextOverlay(title, content string) {
	m.textTitle = title
	w := min(m.w-8, 140)
	h := max(5, m.h-9)
	m.textVP = viewport.New(w-4, h-4)
	m.textVP.SetContent(content)
	m.overlay = overlayText
}

// Update implements tea.Model.
func (m *App) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.w, m.h = msg.Width, msg.Height
		m.ready = true
		m.recalcLayout()
		m.refreshChat(true)
		return m, nil

	case spinner.TickMsg:
		var cmd tea.Cmd
		m.spin, cmd = m.spin.Update(msg)
		if a := m.mgr.ActiveAgent(); a != nil && a.Status.Busy() {
			m.refreshChat(false)
		}
		return m, cmd

	case agentEventMsg:
		return m.handleAgentEvent(msg.ev)

	case mcpConnectedMsg:
		if msg.err != nil {
			m.setFlash(fmt.Sprintf("mcp %s failed: %v", msg.name, msg.err), true)
		} else {
			m.setFlash(fmt.Sprintf("mcp %s connected (%d tools)", msg.name, msg.tools), false)
		}
		return m, m.flashCmd()

	case modelsLoadedMsg:
		if len(msg.items) == 0 {
			e := "no models found"
			if len(msg.errs) > 0 {
				e = strings.Join(msg.errs, " · ")
			}
			m.setFlash(e, true)
			return m, m.flashCmd()
		}
		m.pick = newPicker(pickModel, "switch model", msg.items)
		m.overlay = overlayPicker
		return m, nil

	case compactDoneMsg:
		a := m.mgr.ByID(msg.agentID)
		if a == nil {
			return m, nil
		}
		if a.Status == agent.StatusWaiting {
			a.Status = agent.StatusIdle
		}
		a.StatusDetail = ""
		if msg.err != nil {
			a.CompactFailed = true
			m.setFlash("compact failed: "+msg.err.Error(), true)
			return m, m.flashCmd()
		}
		a.Messages = msg.msgs
		a.CtxTokens = msg.ctxEst
		m.dirty = true
		m.refreshChat(true)
		m.setFlash(fmt.Sprintf("compacted — kept the last %d turns verbatim", compactKeepTurns), false)
		cmds := []tea.Cmd{m.flashCmd()}
		if a.Status == agent.StatusIdle && len(a.Queue) > 0 {
			next := a.Queue[0]
			a.Queue = a.Queue[1:]
			cmds = append(cmds, m.sendText(a, next))
		}
		return m, tea.Batch(cmds...)

	case editorDoneMsg:
		return m.handleEditorDone(msg)

	case flashClearMsg:
		if msg.seq == m.flashSeq {
			m.flash = ""
		}
		return m, nil

	case startPickerMsg:
		return m, m.openSessionPicker()

	case rewindDoneMsg:
		if msg.err != nil {
			m.setFlash("rewind failed: "+msg.err.Error(), true)
		} else {
			// Every agent's picture of the files is now stale; tell them
			// with their next prompt.
			notice := provider.TextMessage("user",
				"(side note, no reply needed) I rewound the working tree to checkpoint "+msg.id+
					" — file contents may differ from your earlier reads and edits; re-read files before relying on them.")
			notice.Hidden = true
			for _, ag := range m.mgr.Agents {
				ag.Messages = append(ag.Messages, notice)
			}
			m.dirty = true
			m.refreshChat(true)
			m.setFlash("rewound working tree to "+msg.id+" — /rewind undoes this too", false)
		}
		return m, tea.Batch(m.flashCmd(), fetchGitCmd(m.cwd()))

	case autosaveMsg:
		if m.cfg.Autosave && m.dirty {
			m.saveSession(false)
		}
		return m, autosaveTick()

	case gitStatusMsg:
		m.git = msg.info
		return m, nil

	case gitTickMsg:
		if m.gitPanelsVisible() {
			return m, tea.Batch(fetchGitCmd(m.cwd()), gitTick())
		}
		return m, gitTick()

	case tea.KeyMsg:
		return m.handleKey(msg)

	case tea.MouseMsg:
		if handled, cmd := m.handleChatMouse(msg); handled {
			return m, tea.Batch(cmd, m.flashCmd())
		}
		var cmd tea.Cmd
		if m.overlay == overlayText || m.overlay == overlayHelp {
			m.textVP, cmd = m.textVP.Update(msg)
		} else {
			m.vp, cmd = m.vp.Update(msg)
		}
		return m, cmd
	}

	// Everything else (cursor blink, …) goes to the textarea.
	var cmd tea.Cmd
	m.ta, cmd = m.ta.Update(msg)
	return m, cmd
}

func (m *App) handleAgentEvent(ev agent.Event) (tea.Model, tea.Cmd) {
	a := m.mgr.ByID(ev.AgentID)
	if a == nil {
		return m, nil // agent was closed; stop listening
	}
	var cmds []tea.Cmd
	switch ev.Kind {
	case agent.EvStatus:
		a.Status = ev.Status
		a.StatusDetail = ev.Text
	case agent.EvTextDelta:
		a.StreamText += ev.Text
		a.Status = agent.StatusStreaming
	case agent.EvThinkingDelta:
		a.StreamThinking += ev.Text
		a.Status = agent.StatusThinking
	case agent.EvMessage:
		if ev.Message != nil {
			a.Messages = append(a.Messages, *ev.Message)
			if ev.Message.Role == "assistant" {
				a.StreamText = ""
				a.StreamThinking = ""
				a.Stats.Turns++
			}
			m.dirty = true
		}
	case agent.EvToolStart:
		if ev.Tool != nil {
			a.Stats.StartToolCall(ev.Tool.Name, ev.Tool.Args)
			a.Status = agent.StatusTool
			a.StatusDetail = "running " + ev.Tool.Name
		}
	case agent.EvToolEnd:
		if ev.Tool != nil {
			a.Stats.FinishToolCall(ev.Tool.Name, truncate(ev.Tool.Result, 400), ev.Tool.IsError, ev.Tool.Dur)
		}
	case agent.EvUsage:
		if ev.Usage != nil {
			u := ev.Usage
			a.Stats.AddRequest(ev.Model, u.InputTokens, u.OutputTokens, u.CacheReadTokens, u.CacheWriteTokens, ev.Dur)
			a.CtxTokens = u.InputTokens + u.OutputTokens + u.CacheReadTokens + u.CacheWriteTokens
			a.CompactFailed = false
		}
	case agent.EvPermission:
		if ev.Perm != nil {
			m.perms = append(m.perms, permEntry{agentID: ev.AgentID, req: ev.Perm})
			if m.overlay == overlayNone || m.overlay == overlayPicker {
				m.overlay = overlayPerm
			}
		}
	case agent.EvNotice:
		m.setFlash(ev.Text, true)
		cmds = append(cmds, m.flashCmd())
	case agent.EvRetry:
		a.StreamText = ""
		a.StreamThinking = ""
		a.Status = agent.StatusWaiting
		a.RetryAt = time.Now().Add(ev.Delay)
		reason := ""
		if ev.Err != nil {
			reason = truncate(ev.Err.Error(), 24)
		}
		a.RetryNote = fmt.Sprintf("%d/%d · %s", ev.Attempt+1, ev.Max, reason)
	case agent.EvError:
		if ev.Err != nil {
			a.LastErr = ev.Err.Error()
		}
		a.Status = agent.StatusError
		a.StatusDetail = ""
		if a == m.mgr.ActiveAgent() {
			m.setFlash(a.LastErr, true)
			cmds = append(cmds, m.flashCmd())
		}
	case agent.EvDone:
		a.StreamText = ""
		a.StreamThinking = ""
		a.StatusDetail = ""
		if a.Status != agent.StatusError {
			a.Status = agent.StatusIdle
		}
		// Unblock and drop any unanswered permission prompts of this agent.
		kept := m.perms[:0]
		for _, e := range m.perms {
			if e.agentID == ev.AgentID {
				select {
				case e.req.Reply <- tools.Decision{}:
				default:
				}
				continue
			}
			kept = append(kept, e)
		}
		m.perms = kept
		if len(m.perms) == 0 && m.overlay == overlayPerm {
			m.overlay = overlayNone
		}
		if m.cfg.Autosave && m.dirty {
			m.saveSession(false)
		}
		if m.gitPanelsVisible() {
			cmds = append(cmds, fetchGitCmd(m.cwd()))
		}
		// Near the context limit: compact before anything else runs.
		if m.cfg.AutoCompact && a.Status == agent.StatusIdle && !a.CompactFailed && len(a.Messages) >= 4 {
			if win := m.contextWindowFor(a); win > 0 && a.CtxTokens >= compactThreshold(win) {
				if cmd := m.compactCmd(a); cmd != nil {
					a.Status = agent.StatusWaiting
					a.StatusDetail = "compacting conversation"
					m.setFlash("context nearly full — compacting older turns", false)
					cmds = append(cmds, cmd, m.flashCmd(), waitAgent(a))
					return m, tea.Batch(cmds...)
				}
			}
		}
		// Send the next queued prompt, if any.
		if a.Status == agent.StatusIdle && len(a.Queue) > 0 {
			next := a.Queue[0]
			a.Queue = a.Queue[1:]
			cmds = append(cmds, m.sendText(a, next))
		}
	}
	if a == m.mgr.ActiveAgent() {
		m.refreshChat(false)
	}
	cmds = append(cmds, waitAgent(a))
	return m, tea.Batch(cmds...)
}

func (m *App) handleEditorDone(msg editorDoneMsg) (tea.Model, tea.Cmd) {
	if msg.err != nil {
		m.setFlash(fmt.Sprintf("editor: %v", msg.err), true)
		return m, m.flashCmd()
	}
	switch msg.target {
	case editTargetPrompt:
		m.ta.SetValue(strings.TrimRight(msg.content, "\n"))
		m.recalcLayout()
		m.setFlash("prompt updated from editor", false)
	case editTargetSystem:
		if a := m.mgr.ActiveAgent(); a != nil {
			a.SystemPrompt = strings.TrimRight(msg.content, "\n")
			m.cfg.SystemPrompt = a.SystemPrompt
			m.saveCfg()
			m.dirty = true
			m.setFlash(fmt.Sprintf("system prompt updated (%s)", humanTokens(len(a.SystemPrompt))), false)
		}
	case editTargetConfig:
		newCfg, err := config.Load(m.cfg.Path())
		if err != nil {
			m.setFlash(err.Error()+" — fix with /settings", true)
			return m, m.flashCmd()
		}
		*m.cfg = *newCfg
		m.keys = NewKeyMap(m.cfg.Keys)
		if m.mgr.Exec != nil {
			m.mgr.Exec.SetMode(m.cfg.PermissionMode)
		}
		m.mgr.ReloadProviders()
		// Push the edited defaults onto existing agents too, so the file is
		// the single source of truth after /settings.
		for _, ag := range m.mgr.Agents {
			ag.MaxTokens = m.cfg.MaxTokens
			ag.Temperature = m.cfg.Temperature
			ag.Thinking = m.cfg.Thinking
			ag.ThinkingBudget = m.cfg.ThinkingBudget
			ag.SystemPrompt = m.cfg.SystemPrompt
		}
		m.recalcLayout()
		m.refreshChat(true)
		m.setFlash("config reloaded", false)
	}
	return m, m.flashCmd()
}

func (m *App) decidePerm(d tools.Decision) {
	if len(m.perms) == 0 {
		return
	}
	e := m.perms[0]
	select {
	case e.req.Reply <- d:
	default:
	}
	m.perms = m.perms[1:]
	if len(m.perms) == 0 {
		m.overlay = overlayNone
	}
}

func (m *App) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	// Permission prompts take absolute priority.
	if m.overlay == overlayPerm && len(m.perms) > 0 {
		switch msg.String() {
		case "y", "Y":
			m.decidePerm(tools.Decision{Allow: true})
		case "a", "A":
			m.decidePerm(tools.Decision{Allow: true, Always: true})
		case "n", "N", "esc", "ctrl+c":
			m.decidePerm(tools.Decision{})
		}
		return m, nil
	}
	if m.overlay == overlayText || m.overlay == overlayHelp {
		switch {
		case m.keys.Is(msg, "scroll_up"):
			m.textVP.SetYOffset(m.textVP.YOffset - 1)
		case m.keys.Is(msg, "scroll_down"):
			m.textVP.SetYOffset(m.textVP.YOffset + 1)
		case m.keys.Is(msg, "page_up"):
			m.textVP.SetYOffset(m.textVP.YOffset - m.textVP.Height)
		case m.keys.Is(msg, "page_down"):
			m.textVP.SetYOffset(m.textVP.YOffset + m.textVP.Height)
		case m.keys.Is(msg, "scroll_top"):
			m.textVP.GotoTop()
		case m.keys.Is(msg, "scroll_bottom"):
			m.textVP.GotoBottom()
		case m.keys.Is(msg, "help") && m.overlay == overlayHelp:
			m.dismissOverlay()
		case m.keys.Is(msg, "cancel"), m.keys.Is(msg, "quit"), msg.String() == "q", m.keys.Is(msg, "send"):
			m.dismissOverlay()
		}
		return m, nil
	}
	if m.overlay == overlayPicker {
		if m.keys.Is(msg, "quit") {
			m.dismissOverlay()
			return m, nil
		}
		// In the panels picker, left/right reorders the selected panel.
		if m.pick.kind == pickPanels {
			if s := msg.String(); s == "left" || s == "right" {
				if it := m.pick.selected(); it != nil {
					m.movePanel(it.value, map[string]int{"left": -1, "right": 1}[s])
				}
				return m, nil
			}
		}
		chosen, closed := m.pick.handleKey(msg)
		if chosen != nil {
			cmd := m.applyPick(*chosen)
			if closed {
				m.dismissOverlay()
			}
			return m, cmd
		}
		if closed {
			m.dismissOverlay()
		}
		return m, nil
	}

	a := m.mgr.ActiveAgent()

	switch {
	case m.keys.Is(msg, "quit"):
		return m.handleCtrlC(a)

	case m.keys.Is(msg, "help"):
		m.openHelp()
		return m, nil

	case m.keys.Is(msg, "cancel"):
		if a != nil && a.Status.Busy() {
			a.Cancel()
			m.setFlash("generation stopped", false)
			return m, m.flashCmd()
		}
		return m, nil

	case m.keys.Is(msg, "focus_next"):
		// With the slash popup open, tab completes instead of moving focus.
		if m.focus == focusInput {
			if ms := m.slashMatches(); len(ms) > 0 {
				sel := ((m.slashSel % len(ms)) + len(ms)) % len(ms)
				m.ta.SetValue("/" + ms[sel].name + " ")
				m.slashSel = 0
				return m, nil
			}
		}
		return m, m.cycleFocus(1)

	case m.keys.Is(msg, "focus_prev"):
		return m, m.cycleFocus(-1)

	case m.keys.Is(msg, "cycle_mode"):
		return m, m.cycleMode()

	case m.keys.Is(msg, "toggle_mouse"):
		m.mouseOn = !m.mouseOn
		if m.mouseOn {
			m.setFlash("mouse captured — wheel scrolls the chat again", false)
			return m, tea.Batch(tea.EnableMouseCellMotion, m.flashCmd())
		}
		m.setFlash("mouse released — select & copy text with your terminal, "+m.keys.FirstKey("toggle_mouse")+" to re-capture", false)
		return m, tea.Batch(tea.DisableMouse, m.flashCmd())

	case m.keys.Is(msg, "toggle_dashboard"):
		m.cfg.Dashboard.Visible = !m.cfg.Dashboard.Visible
		if !m.cfg.Dashboard.Visible && m.focus == focusDash {
			m.focus = focusInput
			m.ta.Focus()
		}
		m.saveCfg()
		m.recalcLayout()
		m.refreshChat(false)
		return m, nil

	case m.keys.Is(msg, "models"):
		m.setFlash("loading models…", false)
		return m, tea.Batch(m.loadModelsCmd(), m.flashCmd())

	case m.keys.Is(msg, "toggle_thinking"):
		if a != nil {
			a.Thinking = !a.Thinking
			m.cfg.Thinking = a.Thinking
			m.saveCfg()
			m.setFlash(fmt.Sprintf("thinking %s", onOff(a.Thinking)), false)
		}
		return m, m.flashCmd()

	case m.keys.Is(msg, "verbose"):
		m.cfg.Verbose = !m.cfg.Verbose
		m.saveCfg()
		m.refreshChat(false)
		m.setFlash(fmt.Sprintf("verbose chat %s", onOff(m.cfg.Verbose)), false)
		return m, m.flashCmd()

	case m.keys.Is(msg, "new_agent"):
		return m, cmdAgent(m, "")

	case m.keys.Is(msg, "next_agent"):
		m.mgr.Next()
		m.refreshChat(true)
		return m, nil

	case m.keys.Is(msg, "prev_agent"):
		m.mgr.Prev()
		m.refreshChat(true)
		return m, nil

	case m.keys.Is(msg, "close_agent"):
		if len(m.mgr.Agents) <= 1 {
			m.setFlash("cannot close the last agent", true)
			return m, m.flashCmd()
		}
		name := a.Name
		m.mgr.Close(m.mgr.Active)
		m.refreshChat(true)
		m.setFlash(fmt.Sprintf("closed %s", name), false)
		return m, m.flashCmd()

	case m.keys.Is(msg, "new_session"):
		m.newSession()
		return m, tea.Batch(waitAgent(m.mgr.ActiveAgent()), m.flashCmd())

	case m.keys.Is(msg, "save_session"):
		m.saveSession(true)
		return m, m.flashCmd()

	case m.keys.Is(msg, "open_session"):
		return m, m.openSessionPicker()

	case m.keys.Is(msg, "edit_prompt"):
		return m, openEditorCmd(m.cfg.Editor, editTargetPrompt, m.ta.Value())

	case m.keys.Is(msg, "edit_system"):
		sys := ""
		if a != nil {
			sys = a.SystemPrompt
		}
		return m, openEditorCmd(m.cfg.Editor, editTargetSystem, sys)
	}

	switch m.focus {
	case focusInput:
		return m.handleInputKey(msg, a)

	case focusChat:
		switch {
		case m.keys.Is(msg, "copy_last"):
			return m, m.copyLast()
		case m.keys.Is(msg, "copy_all"):
			return m, m.copyAll()
		case m.keys.Is(msg, "scroll_up"):
			m.vp.SetYOffset(m.vp.YOffset - 1)
		case m.keys.Is(msg, "scroll_down"):
			m.vp.SetYOffset(m.vp.YOffset + 1)
		case m.keys.Is(msg, "page_up"):
			m.vp.SetYOffset(m.vp.YOffset - m.vp.Height)
		case m.keys.Is(msg, "page_down"):
			m.vp.SetYOffset(m.vp.YOffset + m.vp.Height)
		case m.keys.Is(msg, "scroll_top"):
			m.vp.GotoTop()
		case m.keys.Is(msg, "scroll_bottom"):
			m.vp.GotoBottom()
		}
		return m, nil

	case focusDash:
		return m.handleDashKey(msg, a)
	}
	return m, nil
}

// handleInputKey processes keys while the prompt input is focused: slash
// command completion, prompt history recall, sending, and typing.
func (m *App) handleInputKey(msg tea.KeyMsg, a *agent.Agent) (tea.Model, tea.Cmd) {
	if ms := m.slashMatches(); len(ms) > 0 {
		switch msg.String() {
		case "up":
			m.slashSel--
			if m.slashSel < 0 {
				m.slashSel = len(ms) - 1
			}
			return m, nil
		case "down":
			m.slashSel = (m.slashSel + 1) % len(ms)
			return m, nil
		}
	}

	switch {
	case m.keys.Is(msg, "send"):
		if ms := m.slashMatches(); len(ms) > 0 {
			sel := ((m.slashSel % len(ms)) + len(ms)) % len(ms)
			m.ta.Reset()
			m.slashSel = 0
			m.recalcLayout()
			return m, m.runSlash("/" + ms[sel].name)
		}
		return m, m.submit()
	case m.keys.Is(msg, "newline"):
		m.ta.InsertString("\n")
		m.recalcLayout()
		return m, nil
	}

	// Prompt history recall (shell-style) on up/down when not typing.
	if a != nil {
		s := msg.String()
		if s == "up" && (m.recalling || strings.TrimSpace(m.ta.Value()) == "") {
			prompts := userPrompts(a)
			if len(prompts) > 0 {
				if !m.recalling {
					m.recalling = true
					m.recallIdx = 0
				}
				if m.recallIdx < len(prompts) {
					m.recallIdx++
				}
				m.ta.SetValue(prompts[len(prompts)-m.recallIdx])
				m.recalcLayout()
				return m, nil
			}
		}
		if s == "down" && m.recalling {
			prompts := userPrompts(a)
			m.recallIdx--
			if m.recallIdx <= 0 {
				m.recalling = false
				m.ta.Reset()
			} else if m.recallIdx <= len(prompts) {
				m.ta.SetValue(prompts[len(prompts)-m.recallIdx])
			}
			m.recalcLayout()
			return m, nil
		}
	}

	var cmd tea.Cmd
	before := strings.Count(m.ta.Value(), "\n")
	m.ta, cmd = m.ta.Update(msg)
	if msg.Type == tea.KeyRunes || msg.String() == "backspace" {
		m.recalling = false
	}
	if strings.Count(m.ta.Value(), "\n") != before {
		m.recalcLayout()
	}
	return m, cmd
}

// userPrompts lists the agent's past user prompts, oldest first.
func userPrompts(a *agent.Agent) []string {
	var out []string
	for _, msg := range a.Messages {
		if msg.Role == "user" && !msg.IsToolResult() && !msg.Hidden {
			if t := msg.Text(); strings.TrimSpace(t) != "" {
				out = append(out, t)
			}
		}
	}
	return out
}

// handleCtrlC implements Claude Code-style ctrl+c: stop generation while the
// chat is active; otherwise clear the input, and require a second press
// within quitWindow to exit.
func (m *App) handleCtrlC(a *agent.Agent) (tea.Model, tea.Cmd) {
	if a != nil && a.Status.Busy() {
		a.Cancel()
		m.setFlash("generation stopped", false)
		return m, m.flashCmd()
	}
	if time.Since(m.quitArmed) < quitWindow {
		if m.cfg.Autosave && m.dirty {
			m.saveSession(false)
		}
		return m, tea.Quit
	}
	m.quitArmed = time.Now()
	if m.focus == focusInput && strings.TrimSpace(m.ta.Value()) != "" {
		m.ta.Reset()
		m.recalcLayout()
		m.setFlash("input cleared · press ctrl+c again to exit", false)
	} else {
		m.setFlash("press ctrl+c again to exit", false)
	}
	return m, m.flashCmd()
}

// modeDescriptions explains each permission mode in the flash line.
var modeDescriptions = map[string]string{
	"plan":   "plan mode — read-only; the agent presents plans",
	"manual": "manual mode — every mutating tool call asks",
	"auto":   "auto mode — file edits auto-approved, bash asks",
	"bypass": "bypass mode — all tools run without asking",
}

// permMode is the live permission mode (a launch flag may differ from cfg).
func (m *App) permMode() string {
	if m.mgr.Exec != nil {
		return m.mgr.Exec.Mode()
	}
	return m.cfg.PermissionMode
}

// cycleMode advances plan -> manual -> auto -> bypass and persists it.
func (m *App) cycleMode() tea.Cmd {
	order := tools.Modes
	cur := m.permMode()
	idx := 0
	for i, mode := range order {
		if mode == cur {
			idx = i
			break
		}
	}
	next := order[(idx+1)%len(order)]
	m.cfg.PermissionMode = next
	if m.mgr.Exec != nil {
		m.mgr.Exec.SetMode(next)
	}
	m.saveCfg()
	m.setFlash(modeDescriptions[next], next == "bypass")
	return m.flashCmd()
}

// movePanel shifts a panel earlier/later in the configured order.
func (m *App) movePanel(key string, dir int) {
	panels := m.cfg.Dashboard.Panels
	for i, p := range panels {
		if p != key {
			continue
		}
		j := i + dir
		if j < 0 || j >= len(panels) {
			return
		}
		panels[i], panels[j] = panels[j], panels[i]
		m.saveCfg()
		return
	}
}

// cycleFocus moves focus input -> chat -> dashboard (when visible).
func (m *App) cycleFocus(dir int) tea.Cmd {
	order := []focusArea{focusInput, focusChat}
	if dw, dh := m.dashDims(); m.cfg.Dashboard.Visible && (dw > 0 || dh > 0) {
		order = append(order, focusDash)
	}
	idx := 0
	for i, f := range order {
		if f == m.focus {
			idx = i
			break
		}
	}
	m.focus = order[(idx+dir+len(order))%len(order)]

	if m.focus == focusInput {
		return tea.Batch(m.ta.Focus(), textarea.Blink)
	}
	m.ta.Blur()
	if m.focus == focusDash {
		m.dashEditing = false
	}
	return nil
}

// openPanelsPicker shows the panel toggle/reorder picker.
func (m *App) openPanelsPicker() {
	var items []pickerItem
	for _, d := range panelDefs {
		items = append(items, pickerItem{label: d.title, desc: d.key, value: d.key})
	}
	m.pick = newPicker(pickPanels, "dashboard panels (enter toggles)", items)
	m.overlay = overlayPicker
}

// handleDashKey drives the dashboard: panel-level focus (move/reorder), and
// row editing inside the settings panel.
func (m *App) handleDashKey(msg tea.KeyMsg, a *agent.Agent) (tea.Model, tea.Cmd) {
	defs := m.activePanelDefs()
	if len(defs) == 0 {
		return m, nil
	}
	if m.dashPanelSel >= len(defs) {
		m.dashPanelSel = len(defs) - 1
	}
	if m.dashPanelSel < 0 {
		m.dashPanelSel = 0
	}

	if m.dashEditing {
		n := len(settingsRows)
		switch {
		case m.keys.Is(msg, "cancel"):
			m.dashEditing = false
			return m, nil
		case m.keys.Is(msg, "scroll_up"):
			m.dashSel = (m.dashSel - 1 + n) % n
			return m, nil
		case m.keys.Is(msg, "scroll_down"):
			m.dashSel = (m.dashSel + 1) % n
			return m, nil
		case m.keys.Is(msg, "send"):
			return m, m.activateSetting(a)
		}
		switch msg.String() {
		case "left", "h":
			return m, m.adjustSetting(a, -1)
		case "right", "l":
			return m, m.adjustSetting(a, +1)
		}
		return m, nil
	}

	// Panel level.
	switch {
	case m.keys.Is(msg, "move_panel_prev"):
		m.moveFocusedPanel(defs, -1)
		return m, nil
	case m.keys.Is(msg, "move_panel_next"):
		m.moveFocusedPanel(defs, +1)
		return m, nil
	case m.keys.Is(msg, "panels"):
		m.openPanelsPicker()
		return m, nil
	case m.keys.Is(msg, "send"):
		if defs[m.dashPanelSel].key == "settings" {
			m.dashEditing = true
		} else {
			m.setFlash("settings is the editable panel · shift+arrows move this one · /panels configures", false)
			return m, m.flashCmd()
		}
		return m, nil
	case m.keys.Is(msg, "cancel"):
		m.focus = focusInput
		return m, tea.Batch(m.ta.Focus(), textarea.Blink)
	}
	n := len(defs)
	switch msg.String() {
	case "up", "k", "left", "h":
		m.dashPanelSel = (m.dashPanelSel - 1 + n) % n
	case "down", "j", "right", "l":
		m.dashPanelSel = (m.dashPanelSel + 1) % n
	}
	return m, nil
}

// moveFocusedPanel swaps the focused panel with its neighbor and keeps it
// focused.
func (m *App) moveFocusedPanel(defs []*panelDef, dir int) {
	key := defs[m.dashPanelSel].key
	target := m.dashPanelSel + dir
	if target < 0 || target >= len(defs) {
		return
	}
	m.movePanel(key, dir)
	m.dashPanelSel = target
}

// activateSetting handles enter on a settings row.
func (m *App) activateSetting(a *agent.Agent) tea.Cmd {
	if a == nil {
		return nil
	}
	switch settingsRows[m.dashSel].key {
	case "model":
		m.setFlash("loading models…", false)
		return tea.Batch(m.loadModelsCmd(), m.flashCmd())
	default:
		return m.adjustSetting(a, +1)
	}
}

// adjustSetting changes the selected settings row, applies it to the active
// agent and persists it to the config file.
func (m *App) adjustSetting(a *agent.Agent, dir int) tea.Cmd {
	if a == nil {
		return nil
	}
	switch settingsRows[m.dashSel].key {
	case "model":
		m.setFlash("loading models…", false)
		return tea.Batch(m.loadModelsCmd(), m.flashCmd())
	case "max_tokens":
		a.MaxTokens = max(256, a.MaxTokens+512*dir)
		m.cfg.MaxTokens = a.MaxTokens
	case "temperature":
		t := math.Round((a.Temperature+0.1*float64(dir))*10) / 10
		if t < 0 {
			t = 0
		}
		if t > 2 {
			t = 2
		}
		a.Temperature = t
		m.cfg.Temperature = t
	case "thinking":
		a.Thinking = !a.Thinking
		m.cfg.Thinking = a.Thinking
	case "budget":
		a.ThinkingBudget = max(1024, a.ThinkingBudget+512*dir)
		m.cfg.ThinkingBudget = a.ThinkingBudget
	case "tools":
		a.ToolsEnabled = !a.ToolsEnabled
	case "verbose":
		m.cfg.Verbose = !m.cfg.Verbose
		m.refreshChat(false)
	case "mode":
		return m.cycleMode()
	case "dashpos":
		positions := []string{"right", "left", "top", "bottom"}
		idx := 0
		for i, p := range positions {
			if p == m.cfg.Dashboard.Position {
				idx = i
				break
			}
		}
		m.cfg.Dashboard.Position = positions[((idx+dir)%len(positions)+len(positions))%len(positions)]
		m.saveCfg()
		m.recalcLayout()
		m.refreshChat(false)
		return nil
	}
	m.dirty = true
	m.saveCfg()
	return nil
}

func onOff(b bool) string {
	if b {
		return "on"
	}
	return "off"
}

func (m *App) applyPick(it pickerItem) tea.Cmd {
	switch m.pick.kind {
	case pickModel:
		providerName, model, _ := strings.Cut(it.value, "\x1f")
		a := m.mgr.ActiveAgent()
		if a == nil {
			return nil
		}
		p, err := m.mgr.ProviderFor(providerName)
		if err != nil {
			m.setFlash(err.Error(), true)
			return m.flashCmd()
		}
		a.Provider = p
		a.ProviderName = providerName
		a.ProviderErr = ""
		a.Model = model
		m.cfg.DefaultProvider = providerName
		m.cfg.DefaultModel = model
		m.saveCfg()
		m.dirty = true
		m.setFlash(fmt.Sprintf("%s -> %s", providerName, model), false)
		return m.flashCmd()

	case pickSession:
		return m.loadSessionByID(it.value)

	case pickCheckpoint:
		return m.restoreCheckpoint(it.value)

	case pickPanels:
		key := it.value
		for i, p := range m.cfg.Dashboard.Panels {
			if p == key {
				m.cfg.Dashboard.Panels = append(m.cfg.Dashboard.Panels[:i], m.cfg.Dashboard.Panels[i+1:]...)
				m.saveCfg()
				return nil
			}
		}
		m.cfg.Dashboard.Panels = append(m.cfg.Dashboard.Panels, key)
		m.saveCfg()
		return nil
	}
	return nil
}

func (m *App) loadModelsCmd() tea.Cmd {
	type provEntry struct {
		name string
		cfg  config.ProviderConfig
	}
	var provs []provEntry
	for name, pc := range m.cfg.Providers {
		provs = append(provs, provEntry{name, pc})
	}
	sort.Slice(provs, func(i, j int) bool { return provs[i].name < provs[j].name })

	return func() tea.Msg {
		var items []pickerItem
		var errs []string
		for _, pe := range provs {
			p, err := provider.New(pe.name, pe.cfg)
			if err != nil {
				errs = append(errs, fmt.Sprintf("%s: %v", pe.name, err))
				continue
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			models, err := p.ListModels(ctx)
			cancel()
			if err != nil {
				errs = append(errs, fmt.Sprintf("%s: %v", pe.name, err))
				continue
			}
			for _, model := range models {
				items = append(items, pickerItem{label: model, desc: pe.name, value: pe.name + "\x1f" + model})
			}
		}
		return modelsLoadedMsg{items: items, errs: errs}
	}
}

// submit handles enter on the input: slash commands run, prompts send, and
// prompts typed while the agent is busy are queued.
func (m *App) submit() tea.Cmd {
	a := m.mgr.ActiveAgent()
	if a == nil {
		return nil
	}
	text := strings.TrimSpace(m.ta.Value())
	if text == "" {
		return nil
	}
	m.ta.Reset()
	m.recalling = false
	m.recalcLayout()

	if strings.HasPrefix(text, "/") {
		return m.runSlash(text)
	}
	if a.Status.Busy() {
		a.Queue = append(a.Queue, text)
		m.dirty = true
		m.refreshChat(true)
		m.setFlash(fmt.Sprintf("queued — will send when the agent is done (%d waiting)", len(a.Queue)), false)
		return m.flashCmd()
	}
	return m.sendText(a, text)
}

// sendText appends a user prompt and starts the agent turn.
func (m *App) sendText(a *agent.Agent, text string) tea.Cmd {
	if a.Model == "" {
		// Try to auto-pick the provider's first model.
		if a.Provider != nil {
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			models, err := a.Provider.ListModels(ctx)
			cancel()
			if err == nil && len(models) > 0 {
				a.Model = models[0]
			}
		}
		if a.Model == "" {
			m.setFlash("no model selected — /model picks one", true)
			return m.flashCmd()
		}
	}

	userMsg := provider.TextMessage("user", text)
	a.Messages = append(a.Messages, userMsg)
	if m.sessName == "" {
		m.sessName = truncate(text, 48)
	}
	history := make([]provider.Message, len(a.Messages))
	copy(history, a.Messages)

	a.LastErr = ""
	a.Status = agent.StatusWaiting
	a.StatusDetail = "sending request"
	a.StreamStart = time.Now()
	a.Send(history)

	m.dirty = true
	m.refreshChat(true)
	return m.maybeProbeCtx(a)
}

// maybeProbeCtx asks Ollama for the model's true context window once per
// model, feeding the context gauge for models the catalog doesn't know.
func (m *App) maybeProbeCtx(a *agent.Agent) tea.Cmd {
	if a.Model == "" || m.probedCtx[a.Model] {
		return nil
	}
	if _, ok := catalog.Lookup(a.Model); ok {
		return nil
	}
	ol, isOllama := a.Provider.(*provider.Ollama)
	if !isOllama {
		return nil
	}
	m.probedCtx[a.Model] = true
	model := a.Model
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if n, err := ol.ContextLength(ctx, model); err == nil && n > 0 {
			catalog.Store(model, catalog.Model{Context: n})
		}
		return nil
	}
}

func (m *App) newSession() {
	for i := len(m.mgr.Agents) - 1; i > 0; i-- {
		m.mgr.Close(i)
	}
	if len(m.mgr.Agents) > 0 {
		m.mgr.Agents[0].Cancel()
	}
	m.mgr.Agents = nil
	m.mgr.NewAgent()
	m.mgr.Active = 0
	m.sessID = sessionNewID()
	m.sessName = ""
	m.sessCreated = time.Now()
	m.sessSaved = time.Time{}
	m.dirty = false
	m.ta.Reset()
	m.recalcLayout()
	m.refreshChat(true)
	m.setFlash("new session "+m.sessID, false)
}

func (m *App) saveSession(announce bool) {
	s := m.mgr.ToSession(m.sessID, m.sessName, m.sessCreated)
	if err := s.Save(); err != nil {
		m.setFlash(fmt.Sprintf("save failed: %v", err), true)
		return
	}
	m.sessSaved = time.Now()
	m.dirty = false
	if announce {
		m.setFlash("saved session "+m.sessID, false)
	}
}

func (m *App) setFlash(text string, isErr bool) {
	m.flash = text
	m.flashErr = isErr
	m.flashSeq++
}

func (m *App) flashCmd() tea.Cmd {
	seq := m.flashSeq
	return tea.Tick(4*time.Second, func(time.Time) tea.Msg { return flashClearMsg{seq} })
}

// Layout ------------------------------------------------------------------

// dashDims returns the dashboard's (width, height) claim on the body area.
// Exactly one is non-zero: width for left/right, height for top/bottom.
func (m *App) dashDims() (int, int) {
	if !m.cfg.Dashboard.Visible || m.w < 60 || m.h < 16 {
		return 0, 0
	}
	switch m.cfg.Dashboard.Position {
	case "top", "bottom":
		bodyH := m.h - 3
		h := int(float64(bodyH) * m.cfg.Dashboard.Width)
		if h < 7 {
			h = 7
		}
		if h > bodyH-8 {
			h = bodyH - 8
		}
		if h < 5 {
			return 0, 0
		}
		return 0, h
	default: // right, left
		w := int(float64(m.w) * m.cfg.Dashboard.Width)
		if w < 26 {
			w = 26
		}
		if w > m.w/2 {
			w = m.w / 2
		}
		return w, 0
	}
}

func (m *App) taHeight() int {
	lines := strings.Count(m.ta.Value(), "\n") + 1
	if lines < 1 {
		lines = 1
	}
	if lines > 6 {
		lines = 6
	}
	return lines
}

func (m *App) recalcLayout() {
	if !m.ready {
		return
	}
	bodyH := m.h - 3
	dashW, dashH := m.dashDims()
	chatW := m.w - dashW
	chatH := bodyH - dashH
	innerW := chatW - 2
	taH := m.taHeight()
	m.ta.SetWidth(innerW)
	m.ta.SetHeight(taH)
	m.vp.Width = innerW
	m.vp.Height = max(1, chatH-2-taH-1)
}

func (m *App) refreshChat(force bool) {
	a := m.mgr.ActiveAgent()
	if a == nil || !m.ready {
		return
	}
	atBottom := m.vp.AtBottom()
	content := renderConversation(a, max(10, m.vp.Width-1), m.spin.View(), m.cfg.Verbose)
	// Renderers wrap their own output, but glamour code blocks and wide
	// runes can still overflow; anything wider than the viewport shears the
	// whole layout apart. Hard-wrap as the last line of defense.
	content = wrap.String(content, max(10, m.vp.Width))
	m.chatStyled = strings.Split(content, "\n")
	m.chatPlain = make([]string, len(m.chatStyled))
	for i, l := range m.chatStyled {
		m.chatPlain[i] = stripAnsi(l)
	}
	m.selActive = false
	m.vp.SetContent(content)
	if force || atBottom {
		m.vp.GotoBottom()
	}
}

// View --------------------------------------------------------------------

// View implements tea.Model.
func (m *App) View() string {
	if !m.ready {
		return "loading…"
	}
	title := m.renderTitle()
	tabs := m.renderTabs()
	status := m.renderStatus()
	bodyH := m.h - 3

	var body string
	switch m.overlay {
	case overlayHelp:
		body = m.renderTextOverlay(m.w, bodyH)
	case overlayPerm:
		body = m.renderPerm(m.w, bodyH)
	case overlayText:
		body = m.renderTextOverlay(m.w, bodyH)
	case overlayPicker:
		checked := func(v string) bool {
			for _, p := range m.cfg.Dashboard.Panels {
				if p == v {
					return true
				}
			}
			return false
		}
		body = m.pick.view(m.w, bodyH, checked)
	default:
		body = m.renderBody(bodyH)
	}
	return strings.Join([]string{title, tabs, body, status}, "\n")
}

func (m *App) renderBody(bodyH int) string {
	dashW, dashH := m.dashDims()
	chatW := m.w - dashW
	chatH := bodyH - dashH
	innerW := chatW - 2

	vpView := m.vp.View()
	// Slash command completion popup replaces the bottom of the transcript.
	if ms := m.slashMatches(); len(ms) > 0 && m.focus == focusInput {
		menu := m.renderSlashMenu(ms, innerW)
		vpLines := strings.Split(vpView, "\n")
		menuLines := strings.Split(menu, "\n")
		if len(menuLines) < len(vpLines) {
			vpLines = append(vpLines[:len(vpLines)-len(menuLines)], menuLines...)
			vpView = strings.Join(vpLines, "\n")
		}
	}

	sep := styDim.Render(strings.Repeat("─", max(1, innerW)))
	content := vpView + "\n" + sep + "\n" + m.ta.View()
	chat := borderStyle(m.focus == focusInput || m.focus == focusChat).Width(innerW).Height(chatH - 2).Render(content)

	switch {
	case dashW == 0 && dashH == 0:
		return chat
	case m.cfg.Dashboard.Position == "left":
		return lipgloss.JoinHorizontal(lipgloss.Top, m.renderDashboard(dashW, bodyH), chat)
	case m.cfg.Dashboard.Position == "top":
		return lipgloss.JoinVertical(lipgloss.Left, m.renderDashboardRow(m.w, dashH), chat)
	case m.cfg.Dashboard.Position == "bottom":
		return lipgloss.JoinVertical(lipgloss.Left, chat, m.renderDashboardRow(m.w, dashH))
	default:
		return lipgloss.JoinHorizontal(lipgloss.Top, chat, m.renderDashboard(dashW, bodyH))
	}
}

func (m *App) renderSlashMenu(ms []*slashCmd, w int) string {
	const maxRows = 6
	sel := ((m.slashSel % len(ms)) + len(ms)) % len(ms)
	start := 0
	if sel >= maxRows {
		start = sel - maxRows + 1
	}
	end := min(len(ms), start+maxRows)
	var lines []string
	for i := start; i < end; i++ {
		c := ms[i]
		label := "/" + c.name
		if c.args != "" {
			label += " " + c.args
		}
		if i == sel {
			lines = append(lines, stySelected.Render(padRight(truncate("> "+label+" — "+c.help, w), w)))
		} else {
			lines = append(lines, "  "+styAccent.Render(label)+" "+styDim.Render(truncate(c.help, max(4, w-lipgloss.Width(label)-4))))
		}
	}
	if !zenMode {
		lines = append(lines, styDim.Render("up/down choose · tab complete · enter run"))
	}
	return strings.Join(lines, "\n")
}

func (m *App) renderPerm(w, h int) string {
	if len(m.perms) == 0 {
		return m.renderBody(h)
	}
	e := m.perms[0]
	name := "agent"
	if a := m.mgr.ByID(e.agentID); a != nil {
		name = a.Name
	}
	var b strings.Builder
	b.WriteString(styPanelTitle.Render("permission required") + styDim.Render("  ("+name+")") + "\n\n")
	b.WriteString(styTool.Render("tool: "+e.req.Tool) + "\n")
	summary := sanitizeText(e.req.Summary)
	lines := strings.Split(wrapPlain(summary, min(80, w-16)), "\n")
	if len(lines) > 10 {
		lines = append(lines[:10], styDim.Render("…"))
	}
	for _, l := range lines {
		b.WriteString("  " + styCode.Render(" "+l+" ") + "\n")
	}
	if len(m.perms) > 1 {
		b.WriteString(styDim.Render(fmt.Sprintf("\n(+%d more requests waiting)", len(m.perms)-1)) + "\n")
	}
	b.WriteString("\n" + styOK.Render("[y]") + " allow once   " +
		styWarn.Render("[a]") + " always allow " + e.req.Tool + "   " +
		styErr.Render("[n]") + " deny")
	box := borderStyle(true).Padding(1, 2).Render(b.String())
	return lipgloss.Place(w, h, lipgloss.Center, lipgloss.Center, box)
}

func (m *App) renderTextOverlay(w, h int) string {
	content := styPanelTitle.Render(truncate(m.textTitle, m.textVP.Width)) + "\n" + m.textVP.View()
	if !zenMode {
		content += "\n" + styDim.Render("j/k scroll · u/d page · g/G top/bottom · esc close")
	}
	box := borderStyle(true).Padding(0, 1).Render(content)
	return lipgloss.Place(w, h, lipgloss.Center, lipgloss.Center, box)
}

func (m *App) renderTitle() string {
	name := m.sessName
	if name == "" {
		name = m.sessID
	}
	left := " " + styAppTitle.Render("tapioca") + styDim.Render(" · "+truncate(name, 32))
	if m.mgr.Exec != nil {
		left += styDim.Render(" · " + shortPath(m.mgr.Exec.Cwd()))
	}

	a := m.mgr.ActiveAgent()
	right := ""
	if a != nil {
		model := a.Model
		if model == "" {
			model = "(no model)"
		}
		right = styDim.Render(fmt.Sprintf("%s:%s · effort:%s · maxtok %s ",
			a.ProviderName, model, effortName(a), humanInt(a.MaxTokens)))
	}
	gap := m.w - lipgloss.Width(left) - lipgloss.Width(right)
	if gap < 1 {
		gap = 1
	}
	return left + strings.Repeat(" ", gap) + right
}

func shortPath(p string) string {
	if home, err := os.UserHomeDir(); err == nil && strings.HasPrefix(p, home) {
		p = "~" + strings.TrimPrefix(p, home)
	}
	if len(p) > 32 {
		p = "…" + p[len(p)-31:]
	}
	return p
}

func (m *App) renderTabs() string {
	var tabs []string
	for i, a := range m.mgr.Agents {
		var dot string
		switch {
		case a.Status == agent.StatusError:
			dot = styErr.Render("●")
		case a.Status.Busy():
			dot = styWarn.Render(m.spin.View())
		default:
			dot = styDim.Render("●")
		}
		label := fmt.Sprintf("%d·%s", i+1, a.Name)
		if i == m.mgr.Active {
			tabs = append(tabs, dot+" "+styTabActive.Render(label))
		} else {
			tabs = append(tabs, dot+" "+styTabIdle.Render(label))
		}
	}
	line := " " + strings.Join(tabs, "   ")
	hint := ""
	if !zenMode {
		hint = styDim.Render(m.keys.FirstKey("prev_agent") + "/" + m.keys.FirstKey("next_agent") + " switch · " + m.keys.FirstKey("new_agent") + " new · /fork ")
	}
	gap := m.w - lipgloss.Width(line) - lipgloss.Width(hint)
	if gap < 1 {
		return truncate(line, m.w)
	}
	return line + strings.Repeat(" ", gap) + hint
}

func (m *App) renderStatus() string {
	var left string
	if m.flash != "" {
		if m.flashErr {
			left = " " + styErr.Render(m.flash)
		} else {
			left = " " + styFlash.Render(m.flash)
		}
	} else if zenMode {
		left = " " + styDim.Render("/zen to exit zen")
	} else {
		k := m.keys
		switch m.focus {
		case focusDash:
			left = " " + styDim.Render("dashboard: up/down select · left/right adjust · enter toggle/pick · "+k.FirstKey("panels")+" panels · tab back")
		case focusChat:
			left = " " + styDim.Render("chat: j/k scroll · u/d page · g/G top/bottom · tab next focus")
		default:
			left = " " + styDim.Render(fmt.Sprintf("%s send · / commands · %s newline · %s vim · %s verbose · %s help",
				k.FirstKey("send"), k.FirstKey("newline"), k.FirstKey("edit_prompt"),
				k.FirstKey("verbose"), k.FirstKey("help")))
		}
	}

	var chipSty lipgloss.Style
	switch m.permMode() {
	case "plan":
		chipSty = styAccent
	case "auto":
		chipSty = styWarn
	case "bypass":
		chipSty = styErr
	default:
		chipSty = styDim
	}
	right := chipSty.Render("["+m.permMode()+"]") + " "
	if a := m.mgr.ActiveAgent(); a != nil {
		st := statusLabel(a)
		if a.Status.Busy() {
			if a.Status == agent.StatusThinking && len(a.StreamThinking) > 0 {
				st += fmt.Sprintf(" · %s", humanTokens(len(a.StreamThinking)))
			}
			el := time.Since(a.StreamStart).Seconds()
			rate := ""
			if el > 0.5 && len(a.StreamText)+len(a.StreamThinking) > 0 {
				rate = fmt.Sprintf(" · %.0f tok/s", float64(len(a.StreamText)+len(a.StreamThinking))/4.0/el)
			}
			right += styWarn.Render(m.spin.View()+" "+st+rate) + " "
		} else if a.Status == agent.StatusError {
			right += styErr.Render(st) + " "
		} else {
			right += styDim.Render(st) + " "
		}
	}
	gap := m.w - lipgloss.Width(left) - lipgloss.Width(right)
	if gap < 1 {
		return truncate(left, m.w)
	}
	return left + strings.Repeat(" ", gap) + right
}
