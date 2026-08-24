package ui

import (
	"strings"

	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
)

// Action is one rebindable command.
type Action struct {
	Name  string
	Help  string
	Keys  string // default keys, comma separated; empty = unbound
	Group string
}

// actions is the full list of rebindable actions, grouped for the help view.
// Defaults follow the user's chosen scheme: slash commands cover the rare
// operations, ctrl keys the frequent ones, no alt anywhere.
var actions = []Action{
	{"send", "send prompt", "enter", "chat"},
	// shift+enter works because the view asks for keyboard enhancements and
	// Bubbletea v2 decodes them; a terminal without that support falls back to
	// ctrl+j, which is the same key on the wire and needs no protocol.
	{"newline", "insert newline", "shift+enter,ctrl+j", "chat"},
	{"edit_prompt", "edit prompt in vim/$EDITOR", "ctrl+g", "chat"},
	{"edit_system", "edit system prompt (use /systemprompt)", "", "chat"},
	{"cancel", "stop generation / close overlay", "esc", "chat"},
	{"quit", "stop chat" + gl.sep + "clear input" + gl.sep + "twice = exit", "ctrl+c", "chat"},
	{"verbose", "toggle verbose chat (full thoughts/tools)", "ctrl+o", "chat"},
	{"paste_image", "attach an image from the clipboard", "ctrl+v", "chat"},

	{"models", "pick provider & model (also /model)", "", "model"},
	{"toggle_thinking", "toggle thinking mode", "ctrl+t", "model"},
	{"cycle_mode", "cycle permission mode (plan/manual/auto/bypass)", "shift+tab", "model"},

	{"new_agent", "new agent", "ctrl+n", "agents"},
	{"next_agent", "next agent", "ctrl+right", "agents"},
	{"prev_agent", "previous agent", "ctrl+left", "agents"},
	{"close_agent", "close agent", "ctrl+w", "agents"},

	{"new_session", "new session (use /new)", "", "session"},
	{"save_session", "save session (use /save)", "", "session"},
	{"open_session", "resume a session (use /resume)", "", "session"},

	{"toggle_dashboard", "show/hide dashboards", "ctrl+b", "view"},
	{"panels", "panel picker (also /panels)", "", "view"},
	{"move_panel_prev", "move focused panel up/left (dashboard)", "shift+up,shift+left", "view"},
	{"move_panel_next", "move focused panel down/right (dashboard)", "shift+down,shift+right", "view"},
	{"focus_next", "cycle focus input/chat/dashboard (click also focuses)", "tab", "view"},
	{"focus_prev", "cycle focus backwards", "", "view"},
	{"toggle_mouse", "release mouse for native selection", "", "view"},
	{"help", "toggle help (also /help)", "f1", "view"},

	{"scroll_up", "scroll up / select up", "up,k", "scroll"},
	{"scroll_down", "scroll down / select down", "down,j", "scroll"},
	{"page_up", "page up (chat focus)", "pgup,u", "scroll"},
	{"page_down", "page down (chat focus)", "pgdown,d", "scroll"},
	{"output_up", "scroll output up (any focus)", "pgup", "scroll"},
	{"output_down", "scroll output down (any focus)", "pgdown", "scroll"},
	{"scroll_top", "go to top (chat focus)", "home,g", "scroll"},
	{"scroll_bottom", "go to bottom (chat focus)", "end,G", "scroll"},
	{"search", "search the transcript", "ctrl+f", "scroll"},
	{"copy_last", "copy last response (chat focus)", "y", "scroll"},
	{"copy_all", "copy whole transcript (chat focus)", "Y", "scroll"},
}

// KeyMap resolves actions to key bindings, applying config overrides.
type KeyMap struct {
	bindings map[string]key.Binding
}

// NewKeyMap builds the keymap; overrides maps action name to comma-separated
// key names.
func NewKeyMap(overrides map[string]string) *KeyMap {
	km := &KeyMap{bindings: map[string]key.Binding{}}
	for _, a := range actions {
		keys := a.Keys
		if ov, ok := overrides[a.Name]; ok {
			keys = strings.TrimSpace(ov)
		}
		var parts []string
		for _, k := range strings.Split(keys, ",") {
			if k = strings.TrimSpace(k); k != "" {
				parts = append(parts, k)
			}
		}
		if len(parts) == 0 {
			km.bindings[a.Name] = key.NewBinding(key.WithDisabled())
			continue
		}
		km.bindings[a.Name] = key.NewBinding(key.WithKeys(parts...), key.WithHelp(parts[0], a.Help))
	}
	return km
}

// Is reports whether msg triggers the named action.
func (k *KeyMap) Is(msg tea.KeyPressMsg, action string) bool {
	b, ok := k.bindings[action]
	if !ok {
		return false
	}
	return key.Matches(msg, b)
}

// KeysFor returns the display string for an action's current keys.
func (k *KeyMap) KeysFor(action string) string {
	b, ok := k.bindings[action]
	if !ok {
		return ""
	}
	return strings.Join(b.Keys(), ", ")
}

// FirstKey returns the primary key for an action (for hint lines).
func (k *KeyMap) FirstKey(action string) string {
	b, ok := k.bindings[action]
	if !ok || len(b.Keys()) == 0 {
		return ""
	}
	return b.Keys()[0]
}
