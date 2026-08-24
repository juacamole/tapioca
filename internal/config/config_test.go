package config

import (
	"os"
	"path/filepath"
	"testing"
)

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// Turning every panel off has to survive a restart; only a config that never
// mentions panels should get the defaults.
func TestExplicitlyEmptyPanelsSurviveReload(t *testing.T) {
	cfg, err := Load(writeConfig(t, "[dashboard]\nvisible = true\npanels = []\n"))
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Dashboard.Panels) != 0 {
		t.Errorf("panels = [] was refilled with %v", cfg.Dashboard.Panels)
	}
}

func TestMissingPanelsGetDefaults(t *testing.T) {
	cfg, err := Load(writeConfig(t, "[dashboard]\nvisible = true\n"))
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Dashboard.Panels) == 0 {
		t.Error("a config without a panels key should get the defaults")
	}

	// No [dashboard] table at all is the same case.
	cfg, err = Load(writeConfig(t, "max_tokens = 1234\n"))
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Dashboard.Panels) == 0 {
		t.Error("a config without a dashboard table should get the defaults")
	}
}

func TestPanelsRoundTripThroughSave(t *testing.T) {
	path := writeConfig(t, "[dashboard]\npanels = []\n")
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	reloaded, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(reloaded.Dashboard.Panels) != 0 {
		t.Errorf("save/load resurrected panels: %v", reloaded.Dashboard.Panels)
	}
}

func TestExternalAgentsRoundTrip(t *testing.T) {
	path := writeConfig(t, "[[agents.external]]\nname = \"claude-code\"\n"+
		"command = \"claude\"\nargs = [\"--acp\"]\n")
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Agents.External) != 1 {
		t.Fatalf("agents.external = %+v, want the one entry", cfg.Agents.External)
	}
	got := cfg.Agents.External[0]
	if got.Name != "claude-code" || got.Command != "claude" || len(got.Args) != 1 || got.Args[0] != "--acp" {
		t.Fatalf("entry = %+v", got)
	}
	// A save from inside the app must not drop an agent the file configured:
	// nothing in the defaults would put it back.
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	again, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(again.Agents.External) != 1 || again.Agents.External[0].Command != "claude" {
		t.Fatalf("save dropped the external agent: %+v", again.Agents.External)
	}
}

func TestPermissionsRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/config.toml"
	cfg := Default()
	cfg.path = path
	cfg.Permissions = Permissions{
		Allow: []string{"bash(go test*)"},
		Ask:   []string{"bash(git push*)"},
		Deny:  []string{"read_file(**/.env)"},
	}
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	got, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Permissions.Allow) != 1 || got.Permissions.Allow[0] != "bash(go test*)" ||
		len(got.Permissions.Ask) != 1 || len(got.Permissions.Deny) != 1 {
		t.Fatalf("lost rules on the round trip: %+v", got.Permissions)
	}
	// Saving again must not drop what the first save wrote.
	if err := got.Save(); err != nil {
		t.Fatal(err)
	}
	again, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(again.Permissions.Deny) != 1 {
		t.Fatalf("second save lost rules: %+v", again.Permissions)
	}
}
