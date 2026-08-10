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
