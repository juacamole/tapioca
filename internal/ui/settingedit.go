package ui

import (
	"fmt"
	"math"
	"strconv"
	"strings"

	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"

	"tapioca/internal/agent"
)

// The numeric settings could only be stepped. Reaching 32000 max tokens from
// 4096 meant holding an arrow key through fifty-five presses, and there was no
// way to say what you actually wanted. Stepping stays — it suits a nudge, and
// it is all that makes sense for a toggle or a list of positions — but a
// number you already know should be typeable.

// numericSetting describes a settings row that holds a number.
type numericSetting struct {
	min, max float64
	decimals int // 0 for integers
	unit     string
	apply    func(m *App, a *agent.Agent, v float64)
}

// numericSettings are the rows that accept a typed value. The bounds are what
// the app will honour, so they are named in the error rather than silently
// clamped to: typing 40000 for 4000 should say so, not quietly do something
// else.
var numericSettings = map[string]numericSetting{
	"max_tokens": {
		min: 256, max: 1_000_000, unit: "tokens",
		apply: func(m *App, a *agent.Agent, v float64) {
			a.MaxTokens = int(v)
			m.cfg.MaxTokens = a.MaxTokens
		},
	},
	"budget": {
		min: 1024, max: 200_000, unit: "tokens",
		apply: func(m *App, a *agent.Agent, v float64) {
			a.ThinkingBudget = int(v)
			m.cfg.ThinkingBudget = a.ThinkingBudget
		},
	},
	"temperature": {
		min: 0, max: 2, decimals: 1,
		apply: func(m *App, a *agent.Agent, v float64) {
			t := math.Round(v*10) / 10
			a.Temperature = t
			m.cfg.Temperature = t
		},
	},
}

// settingEdit is an in-progress typed value for one settings row.
type settingEdit struct {
	key   string
	input textinput.Model
	err   string
}

// openSettingInput starts typing into the selected row, seeded with the value
// already there so a small correction does not mean retyping the number.
func (m *App) openSettingInput(key string, current string) {
	in := textinput.New()
	in.Prompt = ""
	in.CharLimit = 12
	in.SetWidth(10)
	in.SetValue(strings.NewReplacer(",", "", " ", "").Replace(current))
	in.CursorEnd()
	in.Focus()
	m.setEdit = &settingEdit{key: key, input: in}
}

// settingInputView is what the row shows while it is being typed into. The
// reason a value was refused goes on the panel's hint line rather than here:
// a settings row is about thirty columns wide, so anything appended to it is
// truncated away, and an error nobody can read is the bug being fixed.
func (m *App) settingInputView() string {
	if m.setEdit == nil {
		return ""
	}
	return m.setEdit.input.View()
}

// commitSettingInput validates what was typed and applies it. It returns an
// error message to show, or "" on success. The typed text is left alone on
// failure so a near miss can be corrected rather than retyped.
func (m *App) commitSettingInput(a *agent.Agent) string {
	if m.setEdit == nil || a == nil {
		return ""
	}
	spec, ok := numericSettings[m.setEdit.key]
	if !ok {
		return ""
	}
	raw := strings.TrimSpace(m.setEdit.input.Value())
	if raw == "" {
		return "enter a number"
	}
	v, err := strconv.ParseFloat(strings.ReplaceAll(raw, ",", ""), 64)
	if err != nil {
		return fmt.Sprintf("%q is not a number", raw)
	}
	if v < spec.min || v > spec.max {
		return fmt.Sprintf("must be between %s and %s", spec.format(spec.min), spec.format(spec.max))
	}
	spec.apply(m, a, v)
	m.dirty = true
	m.saveCfg()
	return ""
}

// format renders a bound the way the row displays values.
func (s numericSetting) format(v float64) string {
	if s.decimals > 0 {
		return strconv.FormatFloat(v, 'f', s.decimals, 64)
	}
	return humanInt(int(v))
}

// handleSettingInput drives the typed field. It owns every key while open, so
// digits cannot leak out and step the value underneath.
func (m *App) handleSettingInput(msg tea.KeyPressMsg, a *agent.Agent) (tea.Model, tea.Cmd) {
	switch {
	case m.keys.Is(msg, "cancel"):
		m.setEdit = nil
		return m, nil
	case m.keys.Is(msg, "send"):
		if errMsg := m.commitSettingInput(a); errMsg != "" {
			m.setEdit.err = errMsg
			// Also flash it. A settings panel is often given fewer rows than it
			// has, so its own hint line — where this would otherwise appear —
			// may not be on screen at all, and an error nobody can read is no
			// better than none. The status line always is, and /log keeps it.
			m.setFlash(errMsg, true)
			return m, m.flashCmd()
		}
		m.setEdit = nil
		return m, nil
	}
	var cmd tea.Cmd
	m.setEdit.input, cmd = m.setEdit.input.Update(msg)
	m.setEdit.err = "" // typing clears the complaint about the last attempt
	return m, cmd
}
