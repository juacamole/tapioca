package ui

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"tapioca/internal/config"
	"tapioca/internal/provider"
)

func llamaStub(t *testing.T, models string) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(models))
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// A llama-server that is listening should be offered, not described as "not
// configured" until someone writes it into a file.
func TestAListeningServerIsFound(t *testing.T) {
	k, _ := provider.KindFor("llamacpp")
	url := llamaStub(t, `{"data":[{"id":"/models/qwen3.gguf"}]}`)
	e := probeOne(k, k.Type, config.ProviderConfig{Type: k.Type, BaseURL: url})
	if e.state != connReady {
		t.Fatalf("state = %v, detail = %q", e.state, e.detail)
	}
	if !strings.Contains(e.detail, "1 model") {
		t.Errorf("detail = %q", e.detail)
	}
}

// Nothing listening is an ordinary machine, not a broken one: a red mark would
// say something was wrong.
func TestNothingListeningIsNotAFailure(t *testing.T) {
	k, _ := provider.KindFor("llamacpp")
	e := detectOne(k)
	if e.state == connFailing {
		t.Errorf("an absent local server was reported as failing: %q", e.detail)
	}
}

// Choosing a found server writes it and switches to it, with no form in
// between — every field it would ask for already has the right value.
func TestChoosingAFoundServerAddsIt(t *testing.T) {
	k, _ := provider.KindFor("llamacpp")
	cfg := config.Default()
	delete(cfg.Providers, "llamacpp")
	m := &App{cfg: cfg, w: 100, h: 30}
	m.addProviderEntry(k)

	pc, ok := m.cfg.Providers["llamacpp"]
	if !ok {
		t.Fatal("choosing a found server did not add it to the config")
	}
	if pc.Type != "llamacpp" || pc.BaseURL != k.DefaultAddress() {
		t.Errorf("wrote %+v", pc)
	}
	if m.overlay == overlayCredential {
		t.Error("a credential form opened for a server that needs nothing asked")
	}
}

// The name belongs to whatever the user already set up under it; taking it
// would silently repoint their provider at this one.
func TestAddingAFoundServerDoesNotTakeAUsedName(t *testing.T) {
	k, _ := provider.KindFor("llamacpp")
	cfg := config.Default()
	cfg.Providers["llamacpp"] = config.ProviderConfig{Type: "openai", BaseURL: "https://example.invalid"}
	m := &App{cfg: cfg, w: 100, h: 30}
	m.addProviderEntry(k)

	if got := m.cfg.Providers["llamacpp"]; got.Type != "openai" {
		t.Errorf("an existing provider was overwritten: %+v", got)
	}
	found := false
	for name, pc := range m.cfg.Providers {
		if name != "llamacpp" && pc.Type == "llamacpp" {
			found = true
		}
	}
	if !found {
		t.Error("the found server was not added under any name")
	}
}
