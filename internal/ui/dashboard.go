package ui

import (
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"

	"tapioca/internal/agent"
)

// panelDef describes one dashboard panel type.
type panelDef struct {
	key    string
	title  string
	weight int
	render func(m *App, a *agent.Agent, w, h int) []string
}

// panelDefs is the registry of available panels; the config picks which are
// shown and in what order.
var panelDefs = []panelDef{
	{"agents", "agents", 2, renderAgentsPanel},
	{"tokens", "tokens", 2, renderTokensPanel},
	{"git", "git", 2, renderGitPanel},
	{"changes", "changes", 3, renderChangesPanel},
	{"tools", "tool calls", 3, renderToolsPanel},
	{"mcp", "mcp servers", 2, renderMCPPanel},
	{"session", "session", 2, renderSessionPanel},
	{"settings", "settings", 3, renderSettingsPanel},
}

func panelByKey(key string) *panelDef {
	for i := range panelDefs {
		if panelDefs[i].key == key {
			return &panelDefs[i]
		}
	}
	return nil
}

// settingsRows are the editable rows of the settings panel, in display order.
var settingsRows = []struct {
	key   string
	label string
}{
	{"model", "model"},
	{"mode", "perm mode"},
	{"max_tokens", "max tokens"},
	{"temperature", "temperature"},
	{"thinking", "thinking"},
	{"budget", "think budget"},
	{"tools", "tools"},
	{"verbose", "verbose chat"},
	{"dashpos", "dash side"},
}

// activePanelDefs resolves the configured panel list.
func (m *App) activePanelDefs() []*panelDef {
	var defs []*panelDef
	for _, key := range m.cfg.Dashboard.Panels {
		if d := panelByKey(key); d != nil {
			defs = append(defs, d)
		}
	}
	return defs
}

// dashLayout computes which panels fit and their sizes along the stacking
// axis. vertical is true for the left/right column layout.
func (m *App) dashLayout(w, h int) (defs []*panelDef, sizes []int, vertical bool) {
	defs = m.activePanelDefs()
	if len(defs) == 0 {
		return nil, nil, true
	}
	switch m.cfg.Dashboard.Position {
	case "top", "bottom":
		const minW = 26
		maxPanels := max(1, w/minW)
		if len(defs) > maxPanels {
			defs = defs[:maxPanels]
		}
		pw := w / len(defs)
		sizes = make([]int, len(defs))
		for i := range defs {
			sizes[i] = pw
		}
		sizes[len(defs)-1] = w - pw*(len(defs)-1)
		return defs, sizes, false
	default:
		const minH = 5
		for len(defs) > 1 && len(defs)*minH > h {
			defs = defs[:len(defs)-1]
		}
		total := 0
		for _, d := range defs {
			total += d.weight
		}
		sizes = make([]int, len(defs))
		used := 0
		extra := h - len(defs)*minH
		if extra < 0 {
			extra = 0
		}
		for i, d := range defs {
			sizes[i] = minH + extra*d.weight/total
			used += sizes[i]
		}
		sizes[len(sizes)-1] += h - used
		return defs, sizes, true
	}
}

// panelBox renders one panel at exactly (w, h) including its border.
func (m *App) panelBox(d *panelDef, a *agent.Agent, w, h int, focused bool) string {
	innerW := w - 2
	innerH := h - 2
	if innerH < 1 {
		innerH = 1
	}
	contentH := innerH - 1 // minus title line
	lines := fitLines(d.render(m, a, innerW, contentH), innerW, contentH)
	title := d.title
	if d.key == "settings" && m.dashEditing && focused {
		title += " · editing"
	}
	content := styPanelTitle.Render(truncate(title, innerW)) + "\n" + strings.Join(lines, "\n")
	return borderStyle(focused).Width(innerW).Height(innerH).Render(content)
}

func (m *App) noPanelsHint(w, h int) string {
	return lipgloss.NewStyle().Width(w).Height(h).Render(
		styDim.Render(" no panels — focus dashboard (tab), then " + m.keys.FirstKey("panels")))
}

// panelFocused reports whether panel index i has dashboard focus.
func (m *App) panelFocused(i, n int) bool {
	if m.focus != focusDash || n == 0 {
		return false
	}
	sel := m.dashPanelSel
	if sel >= n {
		sel = n - 1
	}
	if sel < 0 {
		sel = 0
	}
	return i == sel
}

// renderDashboard renders the stacked panel column at (w, h).
func (m *App) renderDashboard(w, h int) string {
	a := m.mgr.ActiveAgent()
	defs, sizes, _ := m.dashLayout(w, h)
	if len(defs) == 0 {
		return m.noPanelsHint(w, h)
	}
	var boxes []string
	for i, d := range defs {
		boxes = append(boxes, m.panelBox(d, a, w, sizes[i], m.panelFocused(i, len(defs))))
	}
	col := lipgloss.JoinVertical(lipgloss.Left, boxes...)
	return lipgloss.NewStyle().MaxHeight(h).Render(col)
}

// renderDashboardRow lays panels out side by side (top/bottom positions).
func (m *App) renderDashboardRow(w, h int) string {
	a := m.mgr.ActiveAgent()
	defs, sizes, _ := m.dashLayout(w, h)
	if len(defs) == 0 {
		return m.noPanelsHint(w, h)
	}
	var boxes []string
	for i, d := range defs {
		boxes = append(boxes, m.panelBox(d, a, sizes[i], h, m.panelFocused(i, len(defs))))
	}
	return lipgloss.JoinHorizontal(lipgloss.Top, boxes...)
}

// fitLines pads/truncates lines to exactly h rows of width ≤ w.
func fitLines(lines []string, w, h int) []string {
	if len(lines) > h {
		lines = lines[:h]
	}
	out := make([]string, 0, h)
	trunc := lipgloss.NewStyle().MaxWidth(w)
	for _, l := range lines {
		out = append(out, trunc.Render(l))
	}
	for len(out) < h {
		out = append(out, "")
	}
	return out
}

// defaultCosts is the built-in $/Mtok price table, matched by model prefix.
// [costs] entries in the config override it; prices drift, so treat these as
// estimates.
var defaultCosts = []struct {
	prefix  string
	in, out float64
}{
	{"claude-opus-4-5", 5, 25},
	{"claude-opus", 15, 75},
	{"claude-sonnet", 3, 15},
	{"claude-haiku", 1, 5},
	{"gpt-4o-mini", 0.15, 0.6},
	{"gpt-4o", 2.5, 10},
	{"gpt-4.1-mini", 0.4, 1.6},
	{"gpt-4.1", 2, 8},
}

// costFor resolves the price for a model by longest prefix match.
func (m *App) costFor(model string) (in, out float64, ok bool) {
	best := -1
	for prefix, c := range m.cfg.Costs {
		if strings.HasPrefix(model, prefix) && len(prefix) > best {
			best, in, out, ok = len(prefix), c.In, c.Out, true
		}
	}
	if ok {
		return
	}
	for _, c := range defaultCosts {
		if strings.HasPrefix(model, c.prefix) && len(c.prefix) > best {
			best, in, out, ok = len(c.prefix), c.in, c.out, true
		}
	}
	return
}

// sessionCost estimates an agent's spend from its request history.
func (m *App) sessionCost(a *agent.Agent) float64 {
	var total float64
	for _, r := range a.Stats.Requests {
		if in, out, ok := m.costFor(r.Model); ok {
			total += float64(r.In)*in/1e6 + float64(r.Out)*out/1e6
		}
	}
	return total
}

// contextWindowFor returns the assumed context window for the agent's model.
func (m *App) contextWindowFor(a *agent.Agent) int {
	pc := m.cfg.Providers[a.ProviderName]
	if pc.ContextWindow > 0 {
		return pc.ContextWindow
	}
	switch pc.Type {
	case "anthropic":
		return 200_000
	case "openai", "openai-compatible":
		return 128_000
	default:
		return 8_192
	}
}

func renderTokensPanel(m *App, a *agent.Agent, w, h int) []string {
	s := &a.Stats
	lines := []string{
		fmt.Sprintf("%s %s  %s %s",
			styDim.Render("in"), humanInt(s.InputTokens),
			styDim.Render("out"), humanInt(s.OutputTokens)),
		fmt.Sprintf("%s %s · %d reqs",
			styDim.Render("total"), humanInt(s.InputTokens+s.OutputTokens), len(s.Requests)),
	}
	if lr := s.LastRequest(); lr != nil {
		lines = append(lines, fmt.Sprintf("%s %s->%s · %.1fs",
			styDim.Render("last"), humanInt(lr.In), humanInt(lr.Out), lr.Dur.Seconds()))
	}
	if win := m.contextWindowFor(a); win > 0 && a.CtxTokens > 0 {
		pct := a.CtxTokens * 100 / win
		if pct > 100 {
			pct = 100
		}
		barW := max(4, w-14)
		filled := barW * pct / 100
		sty := styOK
		if pct >= 85 {
			sty = styErr
		} else if pct >= 60 {
			sty = styWarn
		}
		bar := sty.Render(strings.Repeat("█", filled)) + styDim.Render(strings.Repeat("░", barW-filled))
		lines = append(lines, fmt.Sprintf("%s %s %d%%", styDim.Render("ctx"), bar, pct))
	}
	if cost := m.sessionCost(a); cost > 0 {
		lines = append(lines, fmt.Sprintf("%s $%.4f", styDim.Render("cost"), cost))
	}
	if a.Status.Busy() && !a.StreamStart.IsZero() {
		el := time.Since(a.StreamStart).Seconds()
		if el > 0.5 {
			estTok := float64(len(a.StreamText)+len(a.StreamThinking)) / 4.0
			lines = append(lines, fmt.Sprintf("%s %.0f tok/s", styDim.Render("live"), estTok/el))
		}
	}
	var outs []int
	for _, r := range s.Requests {
		outs = append(outs, r.Out)
	}
	if len(outs) > 0 {
		lines = append(lines, styAccent.Render(sparkline(outs, w-2))+styDim.Render(" out/req"))
	}
	return lines
}

func renderToolsPanel(m *App, a *agent.Agent, w, h int) []string {
	enabled := styErr.Render("off")
	if a.ToolsEnabled {
		enabled = styOK.Render("on")
	}
	lines := []string{
		fmt.Sprintf("%s tools · agent: %s · %s", humanInt(m.toolCount()), enabled,
			styDim.Render(m.cfg.PermissionMode)),
	}
	tcs := a.Stats.ToolCalls
	if len(tcs) == 0 {
		lines = append(lines, styDim.Render("no tool calls yet"))
		return lines
	}
	for i := len(tcs) - 1; i >= 0 && len(lines) < h; i-- {
		tc := tcs[i]
		var icon string
		switch {
		case tc.Running:
			icon = styWarn.Render(m.spin.View())
		case tc.IsError:
			icon = styErr.Render("x")
		default:
			icon = styOK.Render("+")
		}
		dur := ""
		if !tc.Running {
			dur = fmt.Sprintf(" %.1fs", tc.Dur.Seconds())
		}
		head := fmt.Sprintf("%s %s%s", icon, tc.Name, styDim.Render(dur))
		lines = append(lines, head)
		if len(lines) < h && tc.Args != "" && tc.Args != "{}" {
			lines = append(lines, styDim.Render("  "+truncate(tc.Args, w-3)))
		}
	}
	return lines
}

func renderAgentsPanel(m *App, a *agent.Agent, w, h int) []string {
	var lines []string
	for i, ag := range m.mgr.Agents {
		marker := "  "
		if i == m.mgr.Active {
			marker = styAccent.Render("> ")
		}
		var dot string
		switch {
		case ag.Status == agent.StatusError:
			dot = styErr.Render("●")
		case ag.Status.Busy():
			dot = styWarn.Render("●")
		default:
			dot = styDim.Render("●")
		}
		name := lipgloss.NewStyle().Foreground(agentColor(ag.ID)).Render(ag.Name)
		lines = append(lines, fmt.Sprintf("%s%s %s %s", marker, dot, name,
			styDim.Render(truncate(ag.Model, max(6, w-lipgloss.Width(ag.Name)-8)))))
		lines = append(lines, styDim.Render(fmt.Sprintf("     %s · %s tok · %d msgs",
			statusLabel(ag), humanInt(ag.TotalTokens()), len(ag.Messages))))
	}
	return lines
}

func renderMCPPanel(m *App, a *agent.Agent, w, h int) []string {
	clients := m.mgr.MCP.Clients()
	errs := m.mgr.MCP.Errors()
	if len(clients) == 0 && len(errs) == 0 {
		return []string{
			styDim.Render("no servers configured"),
			styDim.Render("add [[mcp]] entries to config"),
		}
	}
	var lines []string
	for _, c := range clients {
		dot := styOK.Render("●")
		if !c.Alive() {
			dot = styErr.Render("●")
		}
		lines = append(lines, fmt.Sprintf("%s %s %s", dot, c.Name,
			styDim.Render(fmt.Sprintf("(%d tools)", len(c.Tools)))))
		var names []string
		for _, t := range c.Tools {
			names = append(names, t.Name)
		}
		if len(names) > 0 {
			lines = append(lines, styDim.Render("  "+truncate(strings.Join(names, ", "), w-3)))
		}
	}
	for name, e := range errs {
		lines = append(lines, styErr.Render("x "+name))
		lines = append(lines, styDim.Render("  "+truncate(e, w-3)))
	}
	return lines
}

func renderSessionPanel(m *App, a *agent.Agent, w, h int) []string {
	name := m.sessName
	if name == "" {
		name = styDim.Render("(unnamed)")
	}
	saved := "never"
	if !m.sessSaved.IsZero() {
		saved = ago(m.sessSaved)
	}
	dirty := styOK.Render("saved")
	if m.dirty {
		dirty = styWarn.Render("unsaved")
	}
	auto := "off"
	if m.cfg.Autosave {
		auto = "on"
	}
	msgs := 0
	for _, ag := range m.mgr.Agents {
		msgs += len(ag.Messages)
	}
	return []string{
		fmt.Sprintf("%s %s", styDim.Render("id"), m.sessID),
		fmt.Sprintf("%s %s", styDim.Render("name"), truncate(name, w-6)),
		fmt.Sprintf("%s · autosave %s", dirty, auto),
		fmt.Sprintf("%s %s", styDim.Render("saved"), saved),
		styDim.Render(fmt.Sprintf("%d agents · %d msgs", len(m.mgr.Agents), msgs)),
	}
}

// settingValue renders the current value of a settings row.
func (m *App) settingValue(key string, a *agent.Agent) string {
	switch key {
	case "model":
		model := a.Model
		if model == "" {
			model = "(auto)"
		}
		return a.ProviderName + ":" + model
	case "max_tokens":
		return humanInt(a.MaxTokens)
	case "temperature":
		return fmt.Sprintf("%.1f", a.Temperature)
	case "thinking":
		return onOff(a.Thinking)
	case "budget":
		return humanInt(a.ThinkingBudget)
	case "tools":
		return fmt.Sprintf("%s (%d available)", onOff(a.ToolsEnabled), m.toolCount())
	case "verbose":
		return onOff(m.cfg.Verbose)
	case "mode":
		return m.cfg.PermissionMode
	case "dashpos":
		return m.cfg.Dashboard.Position
	}
	return ""
}

func (m *App) toolCount() int {
	n := len(m.mgr.MCP.AllTools())
	if m.mgr.Exec != nil {
		n += len(m.mgr.Exec.Tools())
	}
	return n
}

// renderSettingsPanel renders the editable settings rows. When the dashboard
// is focused the selected row is highlighted and can be changed in place;
// every change is saved to the config file.
func renderSettingsPanel(m *App, a *agent.Agent, w, h int) []string {
	editing := m.focus == focusDash && m.dashEditing
	var lines []string
	for i, r := range settingsRows {
		val := m.settingValue(r.key, a)
		if editing && i == m.dashSel {
			line := fmt.Sprintf("> %-13s %s", r.label, val)
			lines = append(lines, stySelected.Render(padRight(truncate(line, w), w)))
			continue
		}
		lines = append(lines, fmt.Sprintf("  %s %s",
			styDim.Render(fmt.Sprintf("%-13s", r.label)), val))
	}
	if !zenMode {
		switch {
		case editing:
			lines = append(lines, styDim.Render("arrows adjust · enter pick · esc done"))
		case m.focus == focusDash:
			lines = append(lines, styDim.Render("enter to edit · shift+arrows move panel"))
		default:
			lines = append(lines, styDim.Render("tab or click to edit · saved to config"))
		}
	}
	return lines
}

func sparkline(vals []int, w int) string {
	chars := []rune("▁▂▃▄▅▆▇█")
	if w < 1 {
		w = 1
	}
	if len(vals) > w {
		vals = vals[len(vals)-w:]
	}
	maxV := 1
	for _, v := range vals {
		if v > maxV {
			maxV = v
		}
	}
	var b strings.Builder
	for _, v := range vals {
		idx := v * (len(chars) - 1) / maxV
		b.WriteRune(chars[idx])
	}
	return b.String()
}

func ago(t time.Time) string {
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds ago", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return t.Format("Jan 2 15:04")
	}
}
