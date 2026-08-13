package ui

import (
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"

	"tapioca/internal/agent"
	"tapioca/internal/catalog"
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
	{"todos", "plan", 3, renderTodoPanel},
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
	all := d.render(m, a, innerW, contentH)
	off := m.panelScrollFor(d.key, len(all), contentH)
	lines := fitLinesFrom(all, innerW, contentH, off)
	title := d.title
	if d.key == "settings" && m.dashEditing && focused {
		title += "" + gl.sep + "editing"
	}
	// Say when there is more, and which way. Without this a truncated panel is
	// indistinguishable from one that simply has little to show.
	if mark := scrollMark(off, len(all), contentH); mark != "" {
		title += " " + mark
	}
	content := styPanelTitle.Render(truncate(title, innerW)) + "\n" + strings.Join(lines, "\n")
	return borderStyle(focused).Width(innerW).Height(innerH).Render(content)
}

// scrollMark shows which way a panel continues past its edge, or "" when it
// all fits.
func scrollMark(off, total, visible int) string {
	if total <= visible {
		return ""
	}
	switch {
	case off <= 0:
		return gl.moreDown
	case off >= total-visible:
		return gl.moreUp
	default:
		return gl.moreUp + gl.moreDown
	}
}

// panelScrollFor returns a panel's scroll offset, clamped to what its content
// allows. Clamping on read rather than on write means a panel that shrinks —
// a resize, a tool call finishing — cannot be left scrolled past its end.
func (m *App) panelScrollFor(key string, total, visible int) int {
	maxOff := max(0, total-visible)
	off := m.dashScroll[key]
	if off > maxOff {
		off = maxOff
	}
	if off < 0 {
		off = 0
	}
	return off
}

// scrollPanel moves a panel's offset by delta and reports whether it moved.
func (m *App) scrollPanel(key string, delta, total, visible int) bool {
	if total <= visible {
		return false
	}
	before := m.panelScrollFor(key, total, visible)
	if m.dashScroll == nil {
		m.dashScroll = map[string]int{}
	}
	m.dashScroll[key] = before + delta
	return m.panelScrollFor(key, total, visible) != before
}

// revealRow scrolls a panel just far enough to bring a row into view, so
// walking the settings with the arrow keys cannot step onto a row that is not
// on screen.
func (m *App) revealRow(key string, row, total, visible int) {
	if total <= visible {
		return
	}
	if m.dashScroll == nil {
		m.dashScroll = map[string]int{}
	}
	off := m.panelScrollFor(key, total, visible)
	switch {
	case row < off:
		off = row
	case row >= off+visible:
		off = row - visible + 1
	}
	m.dashScroll[key] = off
}

// panelContentHeight is how many lines of a panel's body are on screen, and
// how many it has in total. Both come from the same layout the renderer uses,
// so scrolling can never disagree with what is drawn.
func (m *App) panelContentHeight(defs []*panelDef, i int) (total, visible int) {
	if i < 0 || i >= len(defs) {
		return 0, 0
	}
	dashW, dashH := m.dashDims()
	bodyH := m.h - 3
	var sizes []int
	var w int
	if dashW > 0 {
		_, sizes, _ = m.dashLayout(dashW, bodyH)
		w = dashW
	} else {
		_, sizes, _ = m.dashLayout(m.w, dashH)
	}
	if i >= len(sizes) {
		return 0, 0
	}
	h := sizes[i]
	if dashW == 0 {
		w, h = sizes[i], dashH
	}
	innerW, innerH := w-2, h-2
	if innerH < 1 {
		innerH = 1
	}
	visible = innerH - 1 // minus the title line
	total = len(defs[i].render(m, m.mgr.ActiveAgent(), innerW, visible))
	return total, visible
}

// scrollFocusedPanel scrolls the selected panel by a page.
func (m *App) scrollFocusedPanel(defs []*panelDef, dir int) {
	i := m.dashPanelSel
	total, visible := m.panelContentHeight(defs, i)
	if visible < 1 {
		return
	}
	step := max(1, visible-1) // keep a line of overlap, as a pager does
	m.scrollPanel(defs[i].key, dir*step, total, visible)
}

// revealSettingsRow keeps the row being edited on screen.
func (m *App) revealSettingsRow(defs []*panelDef) {
	i := m.dashPanelSel
	if i < 0 || i >= len(defs) || defs[i].key != "settings" {
		return
	}
	total, visible := m.panelContentHeight(defs, i)
	m.revealRow("settings", m.dashSel, total, visible)
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
	var boxes []string
	for i, d := range defs {
		boxes = append(boxes, m.panelBox(d, a, sizes[i], h, m.panelFocused(i, len(defs))))
	}
	return lipgloss.JoinHorizontal(lipgloss.Top, boxes...)
}

// fitLinesFrom pads and truncates lines to exactly h rows of width ≤ w,
// starting at a scroll offset. Content that did not fit used to be cut off and
// unreachable — the settings panel has ten rows and is routinely given fewer.
func fitLinesFrom(lines []string, w, h, offset int) []string {
	if offset > 0 {
		if offset > len(lines) {
			offset = len(lines)
		}
		lines = lines[offset:]
	}
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

type costRates struct{ In, Out, CacheR, CacheW float64 }

// localProvider reports whether a provider entry is a local backend, whose
// inference is free and whose model names must not match hosted catalog ids.
func (m *App) localProvider(name string) bool {
	t := m.cfg.Providers[name].Type
	return t == "ollama" || t == ""
}

// costFor resolves $/Mtok rates: explicit config overrides always win, the
// hosted catalog and prefix defaults apply only to non-local providers.
func (m *App) costFor(providerName, model string) (costRates, bool) {
	best := -1
	var r costRates
	ok := false
	// A gateway serves "anthropic/claude-opus-5" for what every price list
	// keys as "claude-opus-5", so prefixes are matched against both forms —
	// otherwise nothing through a gateway ever has a price.
	bare := catalog.Bare(model)
	matches := func(prefix string) bool {
		return strings.HasPrefix(model, prefix) || strings.HasPrefix(bare, prefix)
	}
	for prefix, c := range m.cfg.Costs {
		if matches(prefix) && len(prefix) > best {
			best, r, ok = len(prefix), costRates{In: c.In, Out: c.Out}, true
		}
	}
	if !ok && providerName != "" && m.localProvider(providerName) {
		return costRates{}, false
	}
	if !ok {
		if cm, found := catalog.Lookup(model); found && (cm.In > 0 || cm.Out > 0) {
			r, ok = costRates{In: cm.In, Out: cm.Out, CacheR: cm.CacheR, CacheW: cm.CacheW}, true
		}
	}
	if !ok {
		for _, c := range defaultCosts {
			if matches(c.prefix) && len(c.prefix) > best {
				best, r, ok = len(c.prefix), costRates{In: c.in, Out: c.out}, true
			}
		}
	}
	if ok {
		if r.CacheR == 0 {
			r.CacheR = r.In * 0.10
		}
		if r.CacheW == 0 {
			r.CacheW = r.In * 1.25
		}
	}
	return r, ok
}

// sessionCost estimates an agent's spend from its request history.
func (m *App) sessionCost(a *agent.Agent) float64 {
	var total float64
	for _, req := range a.Stats.Requests {
		if r, ok := m.costFor(req.Provider, req.Model); ok {
			total += float64(req.In)*r.In/1e6 + float64(req.Out)*r.Out/1e6 +
				float64(req.CacheR)*r.CacheR/1e6 + float64(req.CacheW)*r.CacheW/1e6
		}
	}
	return total
}

// contextWindowFor returns the context window: config override, then
// probe data (local) or catalog (hosted), then a per-provider-type guess.
func (m *App) contextWindowFor(a *agent.Agent) int {
	pc := m.cfg.Providers[a.ProviderName]
	if pc.ContextWindow > 0 {
		return pc.ContextWindow
	}
	if m.localProvider(a.ProviderName) {
		if lm, ok := catalog.LookupLocal(a.Model); ok && lm.Context > 0 {
			return lm.Context
		}
		return 8192
	}
	if cm, ok := catalog.Lookup(a.Model); ok && cm.Context > 0 {
		return cm.Context
	}
	switch pc.Type {
	case "anthropic":
		return 200_000
	// A gateway fronts models from every vendor, so the type says nothing about
	// the window — the catalog above answers for anything it knows, and this is
	// only reached for a model it has never heard of. 128k errs the right way:
	// too low silently compacts a long session down to nothing, while too high
	// costs one API error that says exactly what happened.
	case "openai", "openai-compatible", "custom", "vercel", "azure":
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
		fmt.Sprintf("%s %s"+gl.sep+"%d reqs",
			styDim.Render("total"), humanInt(s.InputTokens+s.OutputTokens), len(s.Requests)),
	}
	if s.CacheReadTokens > 0 || s.CacheWriteTokens > 0 {
		lines = append(lines, fmt.Sprintf("%s %s read"+gl.sep+"%s write",
			styDim.Render("cache"), humanInt(s.CacheReadTokens), humanInt(s.CacheWriteTokens)))
	}
	if lr := s.LastRequest(); lr != nil {
		lines = append(lines, fmt.Sprintf("%s %s->%s"+gl.sep+"%.1fs",
			styDim.Render("last"), humanInt(lr.In), humanInt(lr.Out), lr.Dur.Seconds()))
	}
	if win := m.contextWindowFor(a); win > 0 && a.CtxTokens > 0 {
		pct := a.CtxTokens * 100 / win
		if pct > 100 {
			pct = 100
		}
		label := fmt.Sprintf(" %d%% of %s", pct, compactTok(win))
		barW := max(4, w-6-len(label))
		filled := barW * pct / 100
		sty := styOK
		if pct >= 85 {
			sty = styErr
		} else if pct >= 60 {
			sty = styWarn
		}
		bar := sty.Render(strings.Repeat(gl.gaugeFull, filled)) + styDim.Render(strings.Repeat(gl.gaugeEmpty, barW-filled))
		lines = append(lines, fmt.Sprintf("%s %s%s", styDim.Render("ctx"), bar, styDim.Render(label)))
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

func renderTodoPanel(m *App, a *agent.Agent, w, h int) []string {
	todos := a.TodoList()
	if len(todos) == 0 {
		return []string{styDim.Render("no plan — the agent writes one for multi-step tasks")}
	}
	done, total := a.TodoProgress()
	lines := []string{fmt.Sprintf("%d/%d done", done, total)}
	for _, t := range todos {
		if len(lines) >= h {
			lines = append(lines[:h-1], styDim.Render(fmt.Sprintf("… %d more", total-h+2)))
			break
		}
		var mark, text string
		switch t.Status {
		case agent.TodoDone:
			mark, text = styOK.Render(gl.todoDone), styDim.Render(t.Content)
		case agent.TodoDoing:
			mark, text = styWarn.Render(gl.todoDoing), styAccent.Render(t.Content)
		default:
			mark, text = styDim.Render(gl.todoWait), t.Content
		}
		lines = append(lines, mark+" "+truncate(text, w-2))
	}
	return lines
}

func renderToolsPanel(m *App, a *agent.Agent, w, h int) []string {
	enabled := styErr.Render("off")
	if a.ToolsEnabled {
		enabled = styOK.Render("on")
	}
	lines := []string{
		fmt.Sprintf("%s tools"+gl.sep+"agent: %s"+gl.sep+"%s", humanInt(m.toolCount()), enabled,
			styDim.Render(m.permMode())),
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
			icon = styErr.Render(gl.toolErr)
		default:
			icon = styOK.Render(gl.toolOK)
		}
		dur := ""
		if !tc.Running {
			dur = fmt.Sprintf(" %.1fs", tc.Dur.Seconds())
		}
		head := fmt.Sprintf("%s %s%s", icon, tc.Name, styDim.Render(dur))
		lines = append(lines, head)
		if len(lines) < h && tc.Args != "" && tc.Args != "{}" {
			lines = append(lines, styDim.Render("  "+truncate(sanitizeText(tc.Args), w-3)))
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
			dot = styErr.Render(gl.dot)
		case ag.Status.Busy():
			dot = styWarn.Render(gl.dot)
		default:
			dot = styDim.Render(gl.dot)
		}
		name := lipgloss.NewStyle().Foreground(agentColor(ag.ID)).Render(ag.Name)
		lines = append(lines, fmt.Sprintf("%s%s %s %s", marker, dot, name,
			styDim.Render(truncate(ag.Model, max(6, w-lipgloss.Width(ag.Name)-8)))))
		lines = append(lines, styDim.Render(fmt.Sprintf("     %s"+gl.sep+"%s tok"+gl.sep+"%d msgs",
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
		dot := styOK.Render(gl.dot)
		if !c.Alive() {
			dot = styErr.Render(gl.dot)
		}
		counts := fmt.Sprintf("(%d tools", len(c.Tools()))
		if n := len(c.Prompts()); n > 0 {
			counts += fmt.Sprintf(", %d prompts", n)
		}
		if n := len(c.Resources()); n > 0 {
			counts += fmt.Sprintf(", %d resources", n)
		}
		lines = append(lines, fmt.Sprintf("%s %s %s", dot, c.Name, styDim.Render(counts+")")))
		var names []string
		for _, t := range c.Tools() {
			names = append(names, sanitizeLabel(t.Name))
		}
		if len(names) > 0 {
			lines = append(lines, styDim.Render("  "+truncate(strings.Join(names, ", "), w-3)))
		}
		if v := c.Version(); v != "" {
			lines = append(lines, styDim.Render("  protocol "+v))
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
		fmt.Sprintf("%s"+gl.sep+"autosave %s", dirty, auto),
		fmt.Sprintf("%s %s", styDim.Render("saved"), saved),
		styDim.Render(fmt.Sprintf("%d agents"+gl.sep+"%d msgs", len(m.mgr.Agents), msgs)),
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
		return m.permMode()
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
			// While a value is being typed the row shows the field, so the
			// number under the cursor is the one that will be applied.
			if m.setEdit != nil && m.setEdit.key == r.key {
				line := fmt.Sprintf("> %-13s %s", r.label, m.settingInputView())
				lines = append(lines, truncate(line, w))
				continue
			}
			line := fmt.Sprintf("> %-13s %s", r.label, val)
			lines = append(lines, stySelected.Render(padRight(truncate(line, w), w)))
			continue
		}
		lines = append(lines, fmt.Sprintf("  %s %s",
			styDim.Render(fmt.Sprintf("%-13s", r.label)), val))
	}
	if !zenMode {
		switch {
		case m.setEdit != nil && editing:
			if m.setEdit.err != "" {
				lines = append(lines, styErr.Render(truncate(m.setEdit.err, w)))
			} else {
				lines = append(lines, styDim.Render("type a number"+gl.sep+"enter saves"+gl.sep+"esc cancels"))
			}
		case editing:
			lines = append(lines, styDim.Render("arrows adjust"+gl.sep+"enter type/pick"+gl.sep+"esc done"))
		case m.focus == focusDash:
			lines = append(lines, styDim.Render("enter to edit"+gl.sep+"shift+arrows move panel"))
		default:
			lines = append(lines, styDim.Render("tab or click to edit"+gl.sep+"saved to config"))
		}
	}
	return lines
}

func compactTok(n int) string {
	switch {
	case n >= 1_000_000:
		return fmt.Sprintf("%.1fM", float64(n)/1e6)
	case n >= 1000:
		return fmt.Sprintf("%dk", n/1000)
	default:
		return fmt.Sprintf("%d", n)
	}
}

func sparkline(vals []int, w int) string {
	chars := gl.spark
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
