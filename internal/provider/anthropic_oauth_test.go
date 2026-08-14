package provider

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"tapioca/internal/config"
)

// stubAnt writes a fake CLI on PATH so the OAuth path can be exercised without
// the real binary and a real browser login.
func stubAnt(t *testing.T, script string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "ant")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+script+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	old := antBinary
	antBinary = path
	t.Cleanup(func() { antBinary = old })
}

// The header swap is the entire protocol difference, and getting it half right
// — a bearer token still accompanied by x-api-key — would authenticate as the
// wrong identity rather than failing.
func TestOAuthSendsBearerAndNotTheKeyHeader(t *testing.T) {
	stubAnt(t, `echo "oat01-test-token"`)

	var got http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"data":[{"id":"claude-opus-5"}]}`))
	}))
	defer srv.Close()

	p, err := NewAnthropic("anthropic", config.ProviderConfig{
		Type: "anthropic", Auth: "oauth", BaseURL: srv.URL,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.ListModels(context.Background()); err != nil {
		t.Fatal(err)
	}

	if got.Get("Authorization") != "Bearer oat01-test-token" {
		t.Errorf("Authorization = %q, want the bearer token", got.Get("Authorization"))
	}
	if got.Get("anthropic-beta") != "oauth-2025-04-20" {
		t.Errorf("anthropic-beta = %q, want oauth-2025-04-20", got.Get("anthropic-beta"))
	}
	if v := got.Get("x-api-key"); v != "" {
		t.Errorf("x-api-key = %q — an OAuth request must not carry a key header", v)
	}
	if got.Get("anthropic-version") == "" {
		t.Error("anthropic-version is missing")
	}
}

// Key auth must be untouched by this change, and must never acquire the oauth
// beta header.
func TestKeyAuthIsUnchanged(t *testing.T) {
	var got http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		w.Write([]byte(`{"data":[]}`))
	}))
	defer srv.Close()

	p, err := NewAnthropic("anthropic", config.ProviderConfig{
		Type: "anthropic", APIKey: "sk-ant-test", BaseURL: srv.URL,
	})
	if err != nil {
		t.Fatal(err)
	}
	p.ListModels(context.Background())

	if got.Get("x-api-key") != "sk-ant-test" {
		t.Errorf("x-api-key = %q", got.Get("x-api-key"))
	}
	if got.Get("Authorization") != "" {
		t.Errorf("key auth sent an Authorization header: %q", got.Get("Authorization"))
	}
	if got.Get("anthropic-beta") != "" {
		t.Errorf("key auth sent the oauth beta header: %q", got.Get("anthropic-beta"))
	}
}

// The documented trap: `print-credentials` with no flags prints the whole JSON
// blob. Sent as a header it yields an empty response rather than an error, so
// it has to be caught here or it surfaces as the model returning nothing.
func TestJSONInsteadOfATokenIsRejected(t *testing.T) {
	stubAnt(t, `echo '{"access_token":"oat01-x","expires_at":"2026-01-01"}'`)
	_, err := oauthToken(context.Background())
	if err == nil {
		t.Fatal("a JSON blob was accepted as a bearer token")
	}
	if !strings.Contains(err.Error(), "JSON") {
		t.Errorf("error does not explain the problem: %v", err)
	}
}

// An absent CLI and an absent login have different fixes, so they must not
// both surface as a generic auth failure.
func TestMissingCLIIsDistinctFromMissingLogin(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	old := antBinary
	antBinary = "ant"
	t.Cleanup(func() { antBinary = old })

	if _, err := oauthToken(context.Background()); err != ErrNoAntCLI {
		t.Errorf("missing CLI reported as %v, want ErrNoAntCLI", err)
	}
	ok, why := AnthropicOAuthAvailable()
	if ok {
		t.Error("reported available with no CLI installed")
	}
	if !strings.Contains(why, "install") {
		t.Errorf("reason %q does not tell the user to install it", why)
	}
}

// A hardcoded brew command is wrong on any machine without Homebrew, which is
// most of them — telling someone to run a command they do not have is the same
// failure as telling them nothing.
func TestInstallHintMatchesTheMachine(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PATH", dir)
	if got := installHint(); !strings.Contains(got, "go install") {
		t.Errorf("with no brew present the hint is %q, want the go command", got)
	}

	// With brew on PATH the tap is the better answer.
	if err := os.WriteFile(filepath.Join(dir, "brew"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got := installHint(); !strings.Contains(got, "brew") {
		t.Errorf("with brew present the hint is %q, want the brew command", got)
	}
}

// The hint must never send a Nix user to nixpkgs for this: `ant` there is
// Apache Ant, a Java build tool sharing the name, which would fail at
// `ant auth login` in a way that looks like a bug in this app.
func TestInstallHintNeverSuggestsTheNixpkgsAnt(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	got := installHint()
	for _, bad := range []string{"nix-shell -p ant", "nix profile install nixpkgs#ant", "pkgs.ant"} {
		if strings.Contains(got, bad) {
			t.Errorf("hint %q suggests %q, which installs Apache Ant", got, bad)
		}
	}
}

func TestLoggedOutSaysHowToLogIn(t *testing.T) {
	stubAnt(t, `echo "no active profile" >&2; exit 1`)
	_, err := oauthToken(context.Background())
	if err == nil {
		t.Fatal("a failing CLI produced no error")
	}
	if !strings.Contains(err.Error(), "ant auth login") {
		t.Errorf("error does not say how to fix it: %v", err)
	}
}

// A stale exported key silently outranks a profile, so a successful login
// looks like it did nothing. That has to be reported, not hidden.
func TestShadowingKeyIsSurfaced(t *testing.T) {
	stubAnt(t, `echo "oat01-test-token"`)
	t.Setenv("ANTHROPIC_API_KEY", "sk-ant-stale")

	ok, why := AnthropicOAuthAvailable()
	if !ok {
		t.Fatalf("reported unavailable despite a working login: %s", why)
	}
	if !strings.Contains(why, "ANTHROPIC_API_KEY") || !strings.Contains(why, "precedence") {
		t.Errorf("the shadowing key was not surfaced: %q", why)
	}
}

// An empty token would be sent as "Bearer " and fail as an empty response.
func TestEmptyTokenIsRejected(t *testing.T) {
	stubAnt(t, `echo ""`)
	if _, err := oauthToken(context.Background()); err == nil {
		t.Fatal("an empty token was accepted")
	}
}

// The token is a credential: it must not be written to the config that gets
// saved to disk.
func TestOAuthWritesNoTokenToConfig(t *testing.T) {
	stubAnt(t, `echo "oat01-secret-token"`)
	cfg := config.ProviderConfig{Type: "anthropic", Auth: "oauth"}
	p, err := NewAnthropic("anthropic", cfg)
	if err != nil {
		t.Fatal(err)
	}
	if p.apiKey != "" {
		t.Errorf("the OAuth client holds a key: %q", p.apiKey)
	}
	if cfg.APIKey != "" || strings.Contains(cfg.APIKeyEnv, "oat01") {
		t.Error("the token reached the provider config")
	}
}

// Apache Ant on PATH must be named as the problem. Its own failures — an
// unknown-argument complaint, or "build.xml does not exist" — are
// unrecognisable as the wrong program to someone who has just been told to
// install "the Anthropic CLI", and read as a bug in this app instead.
func TestApacheAntIsRecognisedAsTheWrongProgram(t *testing.T) {
	stubAnt(t, `
if [ "$1" = "-version" ]; then
  echo "Apache Ant(TM) version 1.10.17 compiled on April 6 2026"
  exit 0
fi
echo "Unknown argument: --access-token" >&2
exit 1`)

	ok, why := AnthropicOAuthAvailable()
	if ok {
		t.Fatal("Apache Ant was accepted as the Anthropic CLI")
	}
	if !strings.Contains(why, "Apache Ant") {
		t.Errorf("reason %q does not name Apache Ant as the problem", why)
	}
	if strings.Contains(why, "Unknown argument") {
		t.Errorf("reason %q passes through Apache Ant's own error instead of explaining it", why)
	}
}
