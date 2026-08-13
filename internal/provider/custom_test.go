package provider

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"tapioca/internal/config"
)

// Each style must put the credential where it says, and nowhere else. A key in
// two places is a key leaked to whichever one was not meant to see it.
func TestEachAuthStylePlacesTheKeyWhereItSays(t *testing.T) {
	cases := []struct {
		style, header, query string
		wantHeader           string
		wantValue            string
		wantQuery            string
	}{
		{style: AuthBearer, wantHeader: "Authorization", wantValue: "Bearer secret-key"},
		{style: AuthHeader, header: "X-API-Key", wantHeader: "X-API-Key", wantValue: "secret-key"},
		{style: AuthQuery, query: "key", wantQuery: "key=secret-key"},
	}
	for _, c := range cases {
		var got http.Header
		var rawQuery string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			got, rawQuery = r.Header.Clone(), r.URL.RawQuery
			w.Write([]byte(`{"data":[]}`))
		}))

		p, err := NewCustom("c", config.ProviderConfig{
			Type: "custom", BaseURL: srv.URL, APIKey: "secret-key",
			AuthStyle: c.style, AuthHeader: c.header, AuthQuery: c.query,
		})
		if err != nil {
			t.Fatalf("%s: %v", c.style, err)
		}
		if _, err := p.ListModels(context.Background()); err != nil {
			t.Fatalf("%s: %v", c.style, err)
		}
		srv.Close()

		if c.wantHeader != "" && got.Get(c.wantHeader) != c.wantValue {
			t.Errorf("%s: %s = %q, want %q", c.style, c.wantHeader, got.Get(c.wantHeader), c.wantValue)
		}
		if c.wantQuery != "" && !strings.Contains(rawQuery, c.wantQuery) {
			t.Errorf("%s: query = %q, want it to contain %q", c.style, rawQuery, c.wantQuery)
		}
		// The key must not also appear anywhere it was not asked for.
		if c.style != AuthBearer && got.Get("Authorization") != "" {
			t.Errorf("%s: also sent Authorization = %q", c.style, got.Get("Authorization"))
		}
		if c.style != AuthQuery && strings.Contains(rawQuery, "secret-key") {
			t.Errorf("%s: the key leaked into the query string: %q", c.style, rawQuery)
		}
	}
}

// A local server on loopback legitimately needs no credential, but that has to
// be chosen rather than being what happens when the key is left blank.
func TestNoneSendsNothingButMustBeChosen(t *testing.T) {
	var got http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		w.Write([]byte(`{"data":[]}`))
	}))
	defer srv.Close()

	p, err := NewCustom("c", config.ProviderConfig{Type: "custom", BaseURL: srv.URL, AuthStyle: AuthNone})
	if err != nil {
		t.Fatal(err)
	}
	p.ListModels(context.Background())
	if got.Get("Authorization") != "" {
		t.Errorf("none sent Authorization = %q", got.Get("Authorization"))
	}

	// Blank key without choosing none is a provider that would fail at the far
	// end with someone else's error message.
	if _, err := NewCustom("c", config.ProviderConfig{Type: "custom", BaseURL: srv.URL}); err == nil {
		t.Error("a missing credential was accepted without auth_style = none")
	}
}

// A header named Authorization in the extras would silently replace the
// credential — sending the request unauthenticated, or as something else.
func TestExtraHeadersCannotReplaceTheCredential(t *testing.T) {
	_, err := NewCustom("c", config.ProviderConfig{
		Type: "custom", BaseURL: "https://example.com", APIKey: "k",
		Headers: map[string]string{"Authorization": "Bearer someone-elses-key"},
	})
	if err == nil {
		t.Fatal("an Authorization header in the extras was accepted")
	}
	if !strings.Contains(err.Error(), "auth_style") {
		t.Errorf("the error does not say what to do instead: %v", err)
	}
}

// A newline in a header would let one entry inject a second header.
func TestHeaderInjectionIsRejected(t *testing.T) {
	for _, h := range []map[string]string{
		{"X-Bad": "value\r\nX-Injected: yes"},
		{"X-Bad\r\nX-Injected": "yes"},
		{"X Bad": "yes"},
	} {
		if _, err := NewCustom("c", config.ProviderConfig{
			Type: "custom", BaseURL: "https://example.com", APIKey: "k", Headers: h,
		}); err == nil {
			t.Errorf("accepted an injectable header: %v", h)
		}
	}
}

// Everything typed into this app goes to this host, so a plaintext address is
// refused unless it is the machine you are sitting at.
func TestPlaintextIsRefusedExceptOnLoopback(t *testing.T) {
	for _, ok := range []string{
		"http://localhost:1234/v1", "http://127.0.0.1:8080", "https://api.example.com/v1",
	} {
		if err := CheckBaseURL(ok); err != nil {
			t.Errorf("CheckBaseURL(%q) = %v, want accepted", ok, err)
		}
	}
	for _, bad := range []string{
		"http://api.example.com/v1", "ftp://example.com", "not a url", "", "/v1/chat",
	} {
		if err := CheckBaseURL(bad); err == nil {
			t.Errorf("CheckBaseURL(%q) was accepted", bad)
		}
	}
}

// A style nobody implements would otherwise fall through to bearer and send
// the credential somewhere the user did not intend.
func TestUnknownAuthStyleIsRejected(t *testing.T) {
	_, err := NewCustom("c", config.ProviderConfig{
		Type: "custom", BaseURL: "https://example.com", APIKey: "k", AuthStyle: "basic",
	})
	if err == nil {
		t.Fatal("an unimplemented auth style was accepted")
	}
	if !strings.Contains(err.Error(), "bearer") {
		t.Errorf("the error does not list the styles that work: %v", err)
	}
}

// The default has to stay bearer, or every existing openai-compatible config
// changes meaning.
func TestBearerIsTheDefault(t *testing.T) {
	var got http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		w.Write([]byte(`{"data":[]}`))
	}))
	defer srv.Close()

	p, err := NewCustom("c", config.ProviderConfig{Type: "custom", BaseURL: srv.URL, APIKey: "k"})
	if err != nil {
		t.Fatal(err)
	}
	p.ListModels(context.Background())
	if got.Get("Authorization") != "Bearer k" {
		t.Errorf("Authorization = %q, want the bearer default", got.Get("Authorization"))
	}
}

// Extra headers are the reason several gateways are usable at all.
func TestExtraHeadersReachTheRequest(t *testing.T) {
	var got http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		w.Write([]byte(`{"data":[]}`))
	}))
	defer srv.Close()

	p, err := NewCustom("c", config.ProviderConfig{
		Type: "custom", BaseURL: srv.URL, APIKey: "k",
		Headers: map[string]string{"X-Org-Id": "org_123", "HTTP-Referer": "https://example.com"},
	})
	if err != nil {
		t.Fatal(err)
	}
	p.ListModels(context.Background())
	if got.Get("X-Org-Id") != "org_123" {
		t.Errorf("X-Org-Id = %q", got.Get("X-Org-Id"))
	}
	if got.Get("Authorization") != "Bearer k" {
		t.Errorf("the credential was lost alongside extra headers: %q", got.Get("Authorization"))
	}
}
