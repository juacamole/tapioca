package agent

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"tapioca/internal/provider"
	"tapioca/internal/tools"
)

// skillWorktree writes one pack into a temporary worktree and returns it. The
// config directory is redirected too, so a personal skill on the machine
// running the tests cannot join the catalogue.
func skillWorktree(t *testing.T, manifest string) string {
	t.Helper()
	t.Setenv("TAPIOCA_CONFIG_DIR", t.TempDir())
	work := t.TempDir()
	dir := filepath.Join(work, ".tapioca", "skills", "release")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	return work
}

const manifest = "---\ndescription: Cut a release end to end.\n---\n\nRun BODY-CANARY before tagging.\n"

// The whole point of the primitive: the description is in every system prompt,
// the instructions behind it are not until something asks for them.
func TestSystemPromptCarriesDescriptionsNotBodies(t *testing.T) {
	work := skillWorktree(t, manifest)
	sys := composeSystem("base", "", tools.NewExecutor(work, tools.ModeManual))

	if !strings.Contains(sys, "Cut a release end to end.") {
		t.Errorf("the catalogue is missing from the system prompt:\n%s", sys)
	}
	if !strings.Contains(sys, "release") {
		t.Errorf("the skill is not named in the system prompt:\n%s", sys)
	}
	if strings.Contains(sys, "BODY-CANARY") {
		t.Errorf("the skill body is in the system prompt, which is what skills exist to avoid:\n%s", sys)
	}
}

func TestNoSkillsMeansNothingInTheSystemPrompt(t *testing.T) {
	t.Setenv("TAPIOCA_CONFIG_DIR", t.TempDir())
	sys := composeSystem("base", "", tools.NewExecutor(t.TempDir(), tools.ModeManual))
	if strings.Contains(strings.ToLower(sys), "skills installed") {
		t.Errorf("a project with no skills paid for a skills section:\n%s", sys)
	}
}

// A tool nobody can use is a paragraph of description bought on every turn.
func TestSkillToolIsOfferedOnlyWhereSkillsExist(t *testing.T) {
	offered := func(t *testing.T, work string) bool {
		t.Helper()
		cp := &captureProvider{req: make(chan provider.Request, 4)}
		a := &Agent{
			ID: 1, Provider: cp, ProviderName: "cap", Model: "m",
			ToolsEnabled: true, Exec: tools.NewExecutor(work, tools.ModeManual),
			Events: make(chan Event, 512),
		}
		a.Send([]provider.Message{provider.TextMessage("user", "hi")})
		req := <-cp.req
		drain(t, a)
		for _, tool := range req.Tools {
			if tool.Name == SkillTool.Name {
				return true
			}
		}
		return false
	}

	t.Run("none installed", func(t *testing.T) {
		t.Setenv("TAPIOCA_CONFIG_DIR", t.TempDir())
		if offered(t, t.TempDir()) {
			t.Error("load_skill was advertised with no skills to load")
		}
	})
	t.Run("one installed", func(t *testing.T) {
		if !offered(t, skillWorktree(t, manifest)) {
			t.Error("load_skill was not advertised where a skill exists")
		}
	})
}

func TestLoadSkillReturnsTheBody(t *testing.T) {
	work := skillWorktree(t, manifest)
	a := &Agent{Exec: tools.NewExecutor(work, tools.ModeManual)}

	text, isErr := a.loadSkill(json.RawMessage(`{"name":"release"}`))
	if isErr {
		t.Fatalf("loading a listed skill failed: %s", text)
	}
	if !strings.Contains(text, "BODY-CANARY") {
		t.Errorf("the instructions were not returned: %q", text)
	}
	if !strings.HasPrefix(text, "[skill: release]") {
		t.Errorf("the result is not labelled, so the transcript cannot say what was loaded: %q", text)
	}
}

func TestLoadSkillReportsWhatIsAvailable(t *testing.T) {
	work := skillWorktree(t, manifest)
	a := &Agent{Exec: tools.NewExecutor(work, tools.ModeManual)}

	text, isErr := a.loadSkill(json.RawMessage(`{"name":"deploy"}`))
	if !isErr {
		t.Fatalf("an unknown skill was accepted: %q", text)
	}
	if !strings.Contains(text, "release") {
		t.Errorf("the model was not told what it could have asked for: %q", text)
	}
	if text, isErr := a.loadSkill(json.RawMessage(`{}`)); !isErr {
		t.Errorf("a call with no name was accepted: %q", text)
	}
}
