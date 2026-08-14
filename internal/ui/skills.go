package ui

import (
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"

	"tapioca/internal/agent"
	"tapioca/internal/provider"
	"tapioca/internal/skills"
)

// Skills are the model's own primitive: it reads the one-line descriptions in
// the system prompt and calls load_skill when one fits. /skills is here for the
// two things the model cannot do — show what is installed, and load one because
// the user already knows which it should be.

// reloadSkills rereads both skill directories; the project half is per
// worktree, so /cd changes the answer.
func (m *App) reloadSkills() { m.skills, m.skillProbs = skills.Load(m.cwd()) }

func cmdSkills(m *App, arg string) tea.Cmd {
	m.reloadSkills()
	if arg == "" {
		m.openSkillsOverlay()
		return nil
	}
	a := m.mgr.ActiveAgent()
	if a == nil || m.busyGuard(a) {
		return m.flashCmd()
	}
	name := strings.ToLower(strings.TrimSpace(arg))
	for _, s := range m.skills {
		if s.Name != name {
			continue
		}
		// Loading twice pays for the same instructions twice, which is the cost
		// this whole primitive exists to avoid.
		if skillsLoaded(a)[s.Name] {
			m.setFlash("skill "+s.Name+" is already in this conversation", false)
			return m.flashCmd()
		}
		body, err := s.Body()
		if err != nil {
			m.setFlash("skill "+s.Name+": "+err.Error(), true)
			return m.flashCmd()
		}
		msg := provider.TextMessage("user", body)
		// Hidden because the transcript already carries the "/skills name" note
		// that asked for this, and a pack's whole body under it would bury the
		// conversation it was loaded to help with.
		msg.Hidden = true
		a.Messages = append(a.Messages, msg)
		m.dirty = true
		m.refreshChat(true)
		m.setFlash("loaded skill "+s.Name+" — the model has it from your next prompt", false)
		return m.flashCmd()
	}
	m.setFlash("no skill named "+truncate(arg, 32)+" — /skills lists them", true)
	return m.flashCmd()
}

// skillsLoaded reports which packs are in this conversation. The transcript is
// the only honest source: a skill counts as loaded because its text is in the
// history, so /clear, compaction and resuming a session all answer correctly
// with nothing to keep in sync. Assistant text is not consulted — a model that
// writes the header itself has not loaded anything.
func skillsLoaded(a *agent.Agent) map[string]bool {
	out := map[string]bool{}
	if a == nil {
		return out
	}
	for _, msg := range a.Messages {
		for _, bl := range msg.Blocks {
			text := ""
			switch {
			case bl.Type == "tool_result" && bl.Name == agent.SkillTool.Name && !bl.IsError:
				text = bl.Content
			case msg.Role == "user" && bl.Type == "text":
				text = bl.Text
			}
			if name, ok := skills.HeaderName(text); ok {
				out[name] = true
			}
		}
	}
	return out
}

func (m *App) openSkillsOverlay() {
	loaded := skillsLoaded(m.mgr.ActiveAgent())
	var b strings.Builder
	if len(m.skills) == 0 {
		b.WriteString(styDim.Render("no skills installed") + "\n\n")
	}
	for _, s := range m.skills {
		where := "personal"
		if s.Project {
			where = "project"
		}
		line := styAccent.Render(s.Name) + styDim.Render(gl.sep+where)
		if loaded[s.Name] {
			line += styOK.Render(gl.sep + "loaded")
		}
		b.WriteString(line + "\n")
		b.WriteString(styDim.Render("  "+s.Description) + "\n")
		if files := s.Files(); len(files) > 0 {
			b.WriteString(styDim.Render("  "+strings.Join(files, ", ")) + "\n")
		}
		b.WriteString("\n")
	}
	if len(m.skillProbs) > 0 {
		b.WriteString(styPanelTitle.Render("skipped") + "\n")
		for _, p := range m.skillProbs {
			b.WriteString("  " + styErr.Render(shortPath(p.Path)) + styDim.Render(gl.sep+p.Reason) + "\n")
		}
		b.WriteString("\n")
	}
	b.WriteString(styDim.Render("the model loads these itself with load_skill" + gl.sep +
		"/skills <name> loads one now"))
	m.openTextOverlay(fmt.Sprintf("skills%s%d installed", gl.sep, len(m.skills)), b.String())
}
