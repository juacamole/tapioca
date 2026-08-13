package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func saveAndRead(t *testing.T, c *Config) string {
	t.Helper()
	dir := t.TempDir()
	c.path = filepath.Join(dir, "config.toml")
	if err := c.Save(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(c.path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// The complaint: a saved config restated every field of every struct.
func TestSavedDefaultsAreSmall(t *testing.T) {
	text := saveAndRead(t, Default())

	for _, unwanted := range []string{
		`base_url = ""`, `api_key = ""`, `auth = ""`, `auth_style = ""`,
		`region = ""`, `profile = ""`, `project = ""`, `credentials_file = ""`,
		`context_window = 0`, `api_version = ""`,
		"[keys]", "[permissions]",
	} {
		if strings.Contains(text, unwanted) {
			t.Errorf("saved config still contains %s", unwanted)
		}
	}
	// A default config should read as a handful of lines, not eighty.
	if n := len(strings.Split(strings.TrimSpace(text), "\n")); n > 25 {
		t.Errorf("a default config is %d lines; it should be a handful", n)
	}
}

// The rule cannot be "drop empty values". These four default to true, so a
// dropped false would turn them back on at the next start — losing a setting
// the user deliberately changed, silently.
func TestFalseValuesThatDifferFromDefaultsSurvive(t *testing.T) {
	c := Default()
	c.Autosave = false
	c.AutoCompact = false
	c.ModelCatalog = false
	c.SandboxNetwork = false
	c.Dashboard.Visible = false

	text := saveAndRead(t, c)
	for _, want := range []string{
		"autosave = false", "auto_compact = false",
		"model_catalog = false", "sandbox_network = false", "visible = false",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("saved config lost %q — it would revert to the default", want)
		}
	}
}

// Zero is a real value for the numeric settings too.
func TestZeroValuesThatDifferFromDefaultsSurvive(t *testing.T) {
	c := Default()
	c.Temperature = 0
	c.ThinkingBudget = 0
	text := saveAndRead(t, c)
	if !strings.Contains(text, "temperature = 0.0") {
		t.Error("an explicit temperature of 0 was dropped")
	}
}

// Providers are user data: one that matches a built-in default must not be
// deleted, because Load only restores the built-ins when the file names no
// providers at all.
func TestDefaultProvidersAreNotDeleted(t *testing.T) {
	c := Default()
	c.Providers["vercel"] = ProviderConfig{Type: "vercel", APIKey: "k"}

	text := saveAndRead(t, c)
	for _, want := range []string{"[providers.ollama]", "[providers.anthropic]", "[providers.vercel]"} {
		if !strings.Contains(text, want) {
			t.Errorf("saved config lost %s", want)
		}
	}
	// ...but their empty fields still go.
	if strings.Contains(text, `region = ""`) {
		t.Error("provider entries still carry empty fields")
	}
}

// The whole thing is only safe if it round-trips. Whatever was configured has
// to come back identical after a save and a load.
func TestSaveLoadRoundTrip(t *testing.T) {
	c := Default()
	c.Autosave = false
	c.SandboxNetwork = false
	c.MaxTokens = 32000
	c.Temperature = 0
	c.Theme = "contrast"
	c.PermissionMode = "auto"
	c.Dashboard.Visible = false
	c.Dashboard.Position = "left"
	c.Dashboard.Panels = []string{"agents", "tokens"}
	c.Providers["custom"] = ProviderConfig{
		Type: "custom", BaseURL: "https://example.com/v1",
		AuthStyle: "header", AuthHeader: "X-Key", APIKey: "secret",
	}
	c.Keys = map[string]string{"newline": "ctrl+j"}
	c.Costs = map[string]Cost{"gpt-4o": {In: 2.5, Out: 10}}
	c.Permissions = Permissions{Allow: []string{"read"}, Deny: []string{"bash(rm)"}}

	dir := t.TempDir()
	c.path = filepath.Join(dir, "config.toml")
	if err := c.Save(); err != nil {
		t.Fatal(err)
	}
	got, err := Load(c.path)
	if err != nil {
		t.Fatal(err)
	}

	checks := []struct {
		name     string
		got, def any
	}{
		{"Autosave", got.Autosave, c.Autosave},
		{"SandboxNetwork", got.SandboxNetwork, c.SandboxNetwork},
		{"MaxTokens", got.MaxTokens, c.MaxTokens},
		{"Theme", got.Theme, c.Theme},
		{"PermissionMode", got.PermissionMode, c.PermissionMode},
		{"Dashboard.Visible", got.Dashboard.Visible, c.Dashboard.Visible},
		{"Dashboard.Position", got.Dashboard.Position, c.Dashboard.Position},
		{"Dashboard.Panels", got.Dashboard.Panels, c.Dashboard.Panels},
		{"Providers", got.Providers, c.Providers},
		{"Keys", got.Keys, c.Keys},
		{"Costs", got.Costs, c.Costs},
		{"Permissions", got.Permissions, c.Permissions},
	}
	for _, ch := range checks {
		if !reflect.DeepEqual(ch.got, ch.def) {
			t.Errorf("%s did not survive the round trip: got %#v, want %#v", ch.name, ch.got, ch.def)
		}
	}
}

// Saving twice must not keep shrinking or growing the file.
func TestSaveIsStable(t *testing.T) {
	c := Default()
	c.MaxTokens = 8192
	dir := t.TempDir()
	c.path = filepath.Join(dir, "config.toml")

	if err := c.Save(); err != nil {
		t.Fatal(err)
	}
	first, _ := os.ReadFile(c.path)

	reloaded, err := Load(c.path)
	if err != nil {
		t.Fatal(err)
	}
	if err := reloaded.Save(); err != nil {
		t.Fatal(err)
	}
	second, _ := os.ReadFile(c.path)

	if string(first) != string(second) {
		t.Errorf("save is not stable:\n--- first ---\n%s\n--- second ---\n%s", first, second)
	}
}

// A default config that is saved and reloaded must still behave like the
// default one — that is the user's "the app looks the way it looks by default".
func TestDefaultRoundTripsToDefault(t *testing.T) {
	dir := t.TempDir()
	c := Default()
	c.path = filepath.Join(dir, "config.toml")
	if err := c.Save(); err != nil {
		t.Fatal(err)
	}
	got, err := Load(c.path)
	if err != nil {
		t.Fatal(err)
	}
	want := Default()
	got.path, want.path = "", ""
	if !reflect.DeepEqual(got, want) {
		t.Errorf("a saved default config did not reload as the default\ngot  %+v\nwant %+v", got, want)
	}
}

// A fresh install used to get two hundred lines of settings already at their
// defaults. The options are still shipped, just beside the config instead of
// inside it.
func TestFreshConfigIsMinimalAndTheExampleIsBesideIt(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	if err := WriteDefault(path); err != nil {
		t.Fatal(err)
	}

	text, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if n := len(strings.Split(strings.TrimSpace(string(text)), "\n")); n > 12 {
		t.Errorf("a fresh config is %d lines:\n%s", n, text)
	}
	for _, unwanted := range []string{"[keys]", "[permissions]", "[providers]", "[dashboard]"} {
		if strings.Contains(string(text), unwanted) {
			t.Errorf("a fresh config still contains %s", unwanted)
		}
	}

	example, err := os.ReadFile(filepath.Join(dir, exampleName))
	if err != nil {
		t.Fatalf("the annotated example was not written beside the config: %v", err)
	}
	for _, want := range []string{"[providers.vercel]", "[permissions]", "bash_allow"} {
		if !strings.Contains(string(example), want) {
			t.Errorf("the example lost its documentation of %s", want)
		}
	}
	if !strings.Contains(string(text), exampleName) {
		t.Error("the fresh config does not say where the options are")
	}
}

// An empty config has to load and behave exactly like the built-in defaults.
func TestAFreshConfigLoadsAsTheDefaults(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	if err := WriteDefault(path); err != nil {
		t.Fatal(err)
	}
	got, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	want := Default()
	got.path, want.path = "", ""
	if !reflect.DeepEqual(got, want) {
		t.Errorf("a fresh config does not load as the defaults\ngot  %+v\nwant %+v", got, want)
	}
}
