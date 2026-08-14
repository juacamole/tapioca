package ui

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"tapioca/internal/agent"
	"tapioca/internal/provider"
	"tapioca/internal/skills"
	"tapioca/internal/tools"
)

const skillManifest = "---\ndescription: Cut a release end to end.\n---\n\nRun BODY-CANARY before tagging.\n"

// skillsApp builds an app whose worktree holds the given packs, written as
// name -> SKILL.md text. The config directory is redirected so personal skills
// on the machine running the tests stay out of it.
func skillsApp(t *testing.T, packs map[string]string) *App {
	t.Helper()
	t.Setenv("TAPIOCA_CONFIG_DIR", t.TempDir())
	work := t.TempDir()
	for name, text := range packs {
		dir := filepath.Join(work, ".tapioca", "skills", name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	m := dashApp(t, 100, 40)
	m.mgr.Exec = tools.NewExecutor(work, tools.ModeManual)
	m.reloadSkills()
	return m
}

// /skills <name> is for when the user already knows which skill applies. The
// body lands in the conversation, hidden, because the "/skills release" note is
// what the transcript should show.
func TestSkillsCommandLoadsOneIntoTheConversation(t *testing.T) {
	m := skillsApp(t, map[string]string{"release": skillManifest})
	a := m.mgr.ActiveAgent()

	cmdSkills(m, "release")

	if len(a.Messages) != 1 {
		t.Fatalf("expected one message, got %+v", a.Messages)
	}
	msg := a.Messages[0]
	if msg.Role != "user" || !msg.Hidden {
		t.Errorf("the loaded skill should be a hidden user message: %+v", msg)
	}
	if !strings.Contains(msg.Text(), "BODY-CANARY") {
		t.Errorf("the instructions were not loaded: %q", msg.Text())
	}
	if name, ok := skills.HeaderName(msg.Text()); !ok || name != "release" {
		t.Errorf("the loaded text is not labelled: %q", msg.Text())
	}
	if m.flashErr {
		t.Errorf("loading a skill reported an error: %q", m.flash)
	}
}

// Paying for the same instructions twice is what the primitive exists to
// avoid, so a second /skills for one already in the conversation adds nothing.
func TestSkillsCommandDoesNotLoadTheSameSkillTwice(t *testing.T) {
	m := skillsApp(t, map[string]string{"release": skillManifest})
	a := m.mgr.ActiveAgent()

	cmdSkills(m, "release")
	cmdSkills(m, "release")

	if len(a.Messages) != 1 {
		t.Fatalf("the skill was loaded %d times", len(a.Messages))
	}
}

func TestSkillsCommandRefusesAnUnknownName(t *testing.T) {
	m := skillsApp(t, map[string]string{"release": skillManifest})
	a := m.mgr.ActiveAgent()

	cmdSkills(m, "deploy")

	if len(a.Messages) != 0 {
		t.Fatalf("an unknown skill still touched the conversation: %+v", a.Messages)
	}
	if !m.flashErr {
		t.Errorf("no error was reported: %q", m.flash)
	}
}

// What is loaded is read back out of the transcript, so /clear, compaction and
// resuming a session answer correctly without anything to keep in sync.
func TestSkillsLoadedReadsTheTranscript(t *testing.T) {
	a := &agent.Agent{ID: 1}
	loaded := skillsLoaded(a)
	if len(loaded) != 0 {
		t.Fatalf("an empty conversation reported %v", loaded)
	}

	a.Messages = append(a.Messages, provider.Message{
		Role: "user",
		Blocks: []provider.Block{{
			Type: "tool_result", Name: agent.SkillTool.Name,
			Content: skills.Header("release") + "\ndirectory: /x\n\ndo the thing",
		}},
	})
	if !skillsLoaded(a)["release"] {
		t.Error("a load by the model was not counted")
	}

	// A model that writes the header itself has loaded nothing, and a failed
	// call put no instructions in front of it either.
	a.Messages = []provider.Message{
		provider.TextMessage("assistant", skills.Header("deploy")+"\nI am pretending"),
		{Role: "user", Blocks: []provider.Block{{
			Type: "tool_result", Name: agent.SkillTool.Name, IsError: true,
			Content: skills.Header("audit") + "\nno such skill",
		}}},
	}
	if got := skillsLoaded(a); len(got) != 0 {
		t.Errorf("assistant text or a failed call counted as loaded: %v", got)
	}
}

// The listing exists so a broken pack is visible instead of mysteriously never
// being used.
func TestSkillsOverlayListsInstalledAndSkipped(t *testing.T) {
	m := skillsApp(t, map[string]string{
		"release": skillManifest,
		"broken":  "no front matter here\n",
	})

	cmdSkills(m, "")

	view := m.textVP.View()
	for _, want := range []string{"release", "Cut a release end to end.", "project", "skipped", "front matter"} {
		if !strings.Contains(view, want) {
			t.Errorf("the skills overlay is missing %q:\n%s", want, view)
		}
	}
	if strings.Contains(view, "BODY-CANARY") {
		t.Errorf("the listing loaded a body:\n%s", view)
	}
}

// A loaded pack is marked as such, so "why is it not using it" has an answer.
func TestSkillsOverlayMarksWhatIsLoaded(t *testing.T) {
	m := skillsApp(t, map[string]string{"release": skillManifest})
	cmdSkills(m, "release")
	cmdSkills(m, "")

	if !strings.Contains(m.textVP.View(), "loaded") {
		t.Errorf("a loaded skill is not marked:\n%s", m.textVP.View())
	}
}
