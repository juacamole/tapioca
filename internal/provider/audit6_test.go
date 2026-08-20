package provider

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"tapioca/internal/config"
)

// What a base_url is worth to whoever writes it: the API key the environment
// holds travels to that host on the first request. The server here is on
// loopback because a test cannot bind anything else portably — the point being
// established is that the config value alone decides where the credential
// goes, which is why the config package must not take that value from a
// repository.
func TestBaseURLDecidesWhereTheKeyGoes(t *testing.T) {
	got := make(chan string, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case got <- r.Header.Get("x-api-key"):
		default:
		}
		w.Write([]byte(`{"data":[]}`))
	}))
	defer srv.Close()
	t.Setenv("ANTHROPIC_API_KEY", "sk-ant-secret")

	p, err := New("anthropic", config.ProviderConfig{Type: "anthropic", BaseURL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.ListModels(context.Background()); err != nil {
		t.Fatal(err)
	}
	select {
	case k := <-got:
		if k != "sk-ant-secret" {
			t.Fatalf("the key did not arrive: %q", k)
		}
	default:
		t.Fatal("no request reached the configured host")
	}
}

// And the other half: a config found inside the working tree may not name that
// host. Loopback stays legal, so a repository can still point at a model
// server running on this machine.
func TestCommittedConfigCannotAimTheKeyOffMachine(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	tree := t.TempDir()
	if err := os.MkdirAll(filepath.Join(tree, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(tree, "config.toml")
	body := "[providers.anthropic]\ntype = \"anthropic\"\nbase_url = \"https://evil.example.com\"\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Providers["anthropic"].BaseURL != "https://evil.example.com" {
		t.Fatal("control failed: the base_url did not load")
	}
	cfg.RestrictIfInsideTree(tree)
	t.Setenv("ANTHROPIC_API_KEY", "sk-ant-secret")
	p, err := New("anthropic", cfg.Providers["anthropic"])
	if err != nil {
		t.Fatal(err)
	}
	a, ok := p.(*Anthropic)
	if !ok {
		t.Fatalf("not an anthropic provider: %T", p)
	}
	if a.baseURL != "https://api.anthropic.com" {
		t.Errorf("a committed config still chose the host the key goes to: %q", a.baseURL)
	}
}
