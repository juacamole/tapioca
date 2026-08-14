package agent

import (
	"encoding/json"
	"strings"

	"tapioca/internal/provider"
	"tapioca/internal/skills"
)

// SkillTool turns the catalogue in the system prompt into something the model
// can act on. It is offered only where skills exist: a tool with nothing to
// load is a paragraph of description bought every turn for nothing.
var SkillTool = provider.ToolDef{
	Name: "load_skill",
	Description: "Load the full instructions behind one of the names listed under Skills in the system prompt. " +
		"Only their descriptions are in context; this returns the rest, along with the files bundled with the " +
		"skill. Call it as soon as a listed skill matches the task, before planning the work, then follow what " +
		"it says.",
	InputSchema: json.RawMessage(`{"type":"object","properties":{"name":{"type":"string","description":"the skill's name, exactly as listed"}},"required":["name"]}`),
}

// loadSkill answers with the pack's body. It needs no permission gate: the
// only thing it can read is a manifest in a skills directory the user or the
// project put there, and the tool takes a name rather than a path, so nothing
// the model writes chooses the file.
func (a *Agent) loadSkill(raw json.RawMessage) (string, bool) {
	var in struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(raw, &in); err != nil || strings.TrimSpace(in.Name) == "" {
		return "invalid arguments: need {\"name\": \"...\"}", true
	}
	cwd := "."
	if a.Exec != nil {
		cwd = a.Exec.Cwd()
	}
	s, ok := skills.Find(cwd, in.Name)
	if !ok {
		list, _ := skills.Load(cwd)
		names := make([]string, 0, len(list))
		for _, s := range list {
			names = append(names, s.Name)
		}
		if len(names) == 0 {
			return "no skills are installed here", true
		}
		return "no such skill — the ones available are: " + strings.Join(names, ", "), true
	}
	body, err := s.Body()
	if err != nil {
		return "the skill could not be read: " + err.Error(), true
	}
	return body, false
}
