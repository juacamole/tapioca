package mcp

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"tapioca/internal/config"
)

// The fakes below are a real authorization server and a real protected
// resource, not stand-ins for this package's own functions: they check what the
// specification requires a client to send and refuse what it requires them to
// refuse, so a test that passes is evidence the client behaved.

// issued is the grant both fakes share — the authorization server mints, the
// resource server checks. Every mint retires what came before, which is what
// makes "the request carried the new token" observable rather than assumed.
type issued struct {
	mu      sync.Mutex
	n       int
	live    map[string]bool
	refresh string
}

func newIssued() *issued { return &issued{live: map[string]bool{}} }

func (i *issued) mint() (access, refresh string) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.n++
	access = fmt.Sprintf("access-%d", i.n)
	refresh = fmt.Sprintf("refresh-%d", i.n)
	i.live = map[string]bool{access: true}
	i.refresh = refresh
	return access, refresh
}

func (i *issued) valid(tok string) bool {
	i.mu.Lock()
	defer i.mu.Unlock()
	return tok != "" && i.live[tok]
}

func (i *issued) currentRefresh() string {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.refresh
}

// asOptions are set before the server starts serving, so a handler goroutine
// can read them without further synchronisation.
type asOptions struct {
	noMetadata     bool   // publish no document; the client must use the defaults
	noRegistration bool   // no registration_endpoint
	issuer         string // override, for the mix-up check
	denyConsent    bool
	refuseRefresh  bool
	plainPKCE      bool  // advertise a PKCE method this client must refuse
	expiresIn      int64 // 0 means an hour
}

type fakeAS struct {
	asOptions
	grants *issued
	srv    *httptest.Server

	mu         sync.Mutex
	registered int
	codes      map[string]string // code -> the challenge it was issued against
	authQuery  url.Values
	tokenForms []url.Values
}

func newFakeAS(t *testing.T, grants *issued, opts asOptions) *fakeAS {
	if opts.expiresIn == 0 {
		opts.expiresIn = 3600
	}
	a := &fakeAS{asOptions: opts, grants: grants, codes: map[string]string{}}
	// Unstarted, so the handlers — which read a.srv for its address — only ever
	// run after the field holds it. Starting first is a data race the race
	// detector is entitled to report.
	a.srv = httptest.NewUnstartedServer(http.HandlerFunc(a.route))
	a.srv.Start()
	t.Cleanup(a.srv.Close)
	return a
}

func (a *fakeAS) URL() string { return a.srv.URL }

func (a *fakeAS) seen() (registrations int, authQuery url.Values, forms []url.Values) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.registered, a.authQuery, append([]url.Values(nil), a.tokenForms...)
}

func (a *fakeAS) route(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/.well-known/oauth-authorization-server", "/.well-known/openid-configuration":
		if a.noMetadata {
			http.NotFound(w, r)
			return
		}
		issuer := a.srv.URL
		if a.issuer != "" {
			issuer = a.issuer
		}
		md := map[string]any{
			"issuer":                           issuer,
			"authorization_endpoint":           a.srv.URL + "/authorize",
			"token_endpoint":                   a.srv.URL + "/token",
			"code_challenge_methods_supported": []string{"S256"},
		}
		if a.plainPKCE {
			md["code_challenge_methods_supported"] = []string{"plain"}
		}
		if !a.noRegistration {
			md["registration_endpoint"] = a.srv.URL + "/register"
		}
		writeJSON(w, md)

	case "/register":
		a.mu.Lock()
		a.registered++
		n := a.registered
		a.mu.Unlock()
		writeJSON(w, map[string]any{"client_id": fmt.Sprintf("client-%d", n)})

	case "/authorize":
		q := r.URL.Query()
		if q.Get("response_type") != "code" || q.Get("client_id") == "" ||
			q.Get("state") == "" || q.Get("code_challenge") == "" ||
			q.Get("code_challenge_method") != "S256" {
			http.Error(w, "the client did not send a usable authorization request", http.StatusBadRequest)
			return
		}
		a.mu.Lock()
		code := fmt.Sprintf("code-%d", len(a.codes)+1)
		a.codes[code] = q.Get("code_challenge")
		a.authQuery = q
		a.mu.Unlock()
		back, err := url.Parse(q.Get("redirect_uri"))
		if err != nil {
			http.Error(w, "bad redirect_uri", http.StatusBadRequest)
			return
		}
		rq := back.Query()
		rq.Set("state", q.Get("state"))
		if a.denyConsent {
			rq.Set("error", "access_denied")
			rq.Set("error_description", "the user pressed no")
		} else {
			rq.Set("code", code)
		}
		back.RawQuery = rq.Encode()
		http.Redirect(w, r, back.String(), http.StatusFound)

	case "/token":
		if err := r.ParseForm(); err != nil {
			http.Error(w, "bad form", http.StatusBadRequest)
			return
		}
		a.mu.Lock()
		a.tokenForms = append(a.tokenForms, r.PostForm)
		challenge := a.codes[r.PostForm.Get("code")]
		a.mu.Unlock()
		switch r.PostForm.Get("grant_type") {
		case "authorization_code":
			if challenge == "" {
				oauthErrJSON(w, "invalid_grant", "no such code")
				return
			}
			// S256, computed here rather than borrowed from the client: a
			// verifier equal to its own challenge is what "plain" looks like,
			// and it must not pass.
			sum := sha256.Sum256([]byte(r.PostForm.Get("code_verifier")))
			if base64.RawURLEncoding.EncodeToString(sum[:]) != challenge {
				oauthErrJSON(w, "invalid_grant", "pkce verification failed")
				return
			}
		case "refresh_token":
			if a.refuseRefresh {
				oauthErrJSON(w, "invalid_grant", "the grant was revoked")
				return
			}
			if r.PostForm.Get("refresh_token") != a.grants.currentRefresh() {
				oauthErrJSON(w, "invalid_grant", "that refresh token was rotated away")
				return
			}
		default:
			oauthErrJSON(w, "unsupported_grant_type", r.PostForm.Get("grant_type"))
			return
		}
		access, refresh := a.grants.mint()
		writeJSON(w, map[string]any{
			"access_token":  access,
			"refresh_token": refresh,
			"token_type":    "Bearer",
			"expires_in":    a.expiresIn,
		})

	default:
		http.NotFound(w, r)
	}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func oauthErrJSON(w http.ResponseWriter, code, desc string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusBadRequest)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": code, "error_description": desc})
}

// protectedServer is an MCP endpoint behind OAuth: everything is a 401 until a
// token it recognises arrives, and then it is an ordinary server.
type protectedServer struct {
	*fakeServer
	grants    *issued
	asURL     string
	metaPath  string // where it publishes RFC 9728 metadata
	challenge bool   // whether its 401 points at that document

	endpoint string // the MCP url, filled in once the server has an address

	hmu  sync.Mutex
	sent []http.Header
}

func (p *protectedServer) handler() http.Handler {
	inner := p.fakeServer.handler()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if p.metaPath != "" && r.URL.Path == p.metaPath {
			writeJSON(w, map[string]any{
				"resource":              "http://" + r.Host + "/mcp",
				"authorization_servers": []string{p.asURL},
				"scopes_supported":      []string{"read", "write"},
			})
			return
		}
		p.hmu.Lock()
		p.sent = append(p.sent, r.Header.Clone())
		p.hmu.Unlock()
		tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !p.grants.valid(tok) {
			if p.challenge {
				w.Header().Set("WWW-Authenticate",
					`Bearer realm="mcp", resource_metadata="http://`+r.Host+p.metaPath+`"`)
			}
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		inner.ServeHTTP(w, r)
	})
}

// headers returns what the last request to the endpoint carried.
func (p *protectedServer) headers() http.Header {
	p.hmu.Lock()
	defer p.hmu.Unlock()
	if len(p.sent) == 0 {
		return http.Header{}
	}
	return p.sent[len(p.sent)-1]
}

// consent stands in for the browser: it follows the redirect chain to the
// loopback listener exactly as one would.
func consent(t *testing.T, target string) {
	t.Helper()
	resp, err := http.Get(target)
	if err != nil {
		t.Fatalf("following the consent redirect: %v", err)
	}
	resp.Body.Close()
}

// oauthEnv wires an authorization server to a protected MCP endpoint and points
// the token store at a temporary directory.
func oauthEnv(t *testing.T, opts asOptions, metaPath string, challenge bool) (*fakeAS, *protectedServer, config.MCPServerConfig) {
	t.Helper()
	t.Setenv("TAPIOCA_CONFIG_DIR", t.TempDir())
	grants := newIssued()
	as := newFakeAS(t, grants, opts)
	prot := &protectedServer{
		fakeServer: &fakeServer{},
		grants:     grants,
		asURL:      as.URL(),
		metaPath:   metaPath,
		challenge:  challenge,
	}
	srv := httptest.NewServer(prot.handler())
	t.Cleanup(srv.Close)
	prot.endpoint = srv.URL + "/mcp"
	return as, prot, config.MCPServerConfig{Name: "remote", URL: prot.endpoint, Auth: "oauth"}
}

// One consent, then the server is usable — and everything the specification
// requires of the client on the way there was actually sent.
func TestLoginThenConnectCarriesTheToken(t *testing.T) {
	as, prot, cfg := oauthEnv(t, asOptions{}, "/.well-known/oauth-protected-resource/mcp", true)

	auth, err := StartLogin(context.Background(), cfg)
	if err != nil {
		t.Fatalf("starting the login: %v", err)
	}
	defer auth.Close()
	if !strings.HasPrefix(auth.URL, as.URL()+"/authorize") {
		t.Fatalf("consent url %q does not point at the authorization server", auth.URL)
	}
	consent(t, auth.URL)
	if err := auth.Wait(context.Background()); err != nil {
		t.Fatalf("completing the login: %v", err)
	}

	registrations, authQuery, forms := as.seen()
	if registrations != 1 {
		t.Errorf("registered %d clients, want 1", registrations)
	}
	if got := authQuery.Get("resource"); got != prot.endpoint {
		t.Errorf("authorize carried resource %q, want the mcp endpoint", got)
	}
	if got := authQuery.Get("scope"); got != "read write" {
		t.Errorf("authorize carried scope %q, want what the resource advertised", got)
	}
	if len(forms) != 1 {
		t.Fatalf("%d token requests, want 1", len(forms))
	}
	if forms[0].Get("code_verifier") == "" {
		t.Error("the token request carried no pkce verifier")
	}
	if forms[0].Get("resource") != prot.endpoint {
		t.Errorf("the token request carried resource %q, want the mcp endpoint", forms[0].Get("resource"))
	}

	c, err := Start(context.Background(), cfg)
	if err != nil {
		t.Fatalf("connecting after the login: %v", err)
	}
	defer c.Close()
	if len(c.Tools()) != 1 {
		t.Fatalf("tools not listed after the login: %+v", c.Tools())
	}
	if got := prot.headers().Get("Authorization"); !strings.HasPrefix(got, "Bearer access-") {
		t.Errorf("the server was sent %q, not the token from the login", got)
	}
	// A second connection must not need a second consent.
	if _, _, forms = as.seen(); len(forms) != 1 {
		t.Errorf("connecting cost another token request: %d in total", len(forms))
	}
}

// The authorization server is found however the MCP server chooses to say where
// it is: a pointer in its challenge, or one of the well-known paths.
func TestAuthorizationServerIsDiscoveredEveryWayItIsPublished(t *testing.T) {
	cases := []struct {
		name      string
		metaPath  string
		challenge bool
	}{
		{"challenge points at an unusual path", "/auth/metadata.json", true},
		{"well-known with the resource path appended", "/.well-known/oauth-protected-resource/mcp", false},
		{"well-known at the root", "/.well-known/oauth-protected-resource", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			as, _, cfg := oauthEnv(t, asOptions{}, tc.metaPath, tc.challenge)
			auth, err := StartLogin(context.Background(), cfg)
			if err != nil {
				t.Fatalf("starting the login: %v", err)
			}
			defer auth.Close()
			if !strings.HasPrefix(auth.URL, as.URL()+"/authorize") {
				t.Fatalf("consent url %q does not point at the authorization server", auth.URL)
			}
		})
	}
}

// A second login reuses the client the first one registered. That is only
// possible because the listener asks for the port the last login used: a
// registered client is tied to the exact redirect URI, port and all.
func TestASecondLoginReusesTheRegisteredClient(t *testing.T) {
	as, _, cfg := oauthEnv(t, asOptions{}, "/.well-known/oauth-protected-resource/mcp", true)
	for range 2 {
		auth, err := StartLogin(context.Background(), cfg)
		if err != nil {
			t.Fatalf("starting the login: %v", err)
		}
		consent(t, auth.URL)
		if err := auth.Wait(context.Background()); err != nil {
			t.Fatalf("completing the login: %v", err)
		}
	}
	if registrations, _, _ := as.seen(); registrations != 1 {
		t.Errorf("registered %d clients over two logins, want 1", registrations)
	}
}

// An authorization server publishing no metadata at all is still usable: the
// specification names default locations for its endpoints.
func TestServerWithoutMetadataUsesTheDefaultEndpoints(t *testing.T) {
	as, _, cfg := oauthEnv(t, asOptions{noMetadata: true}, "/.well-known/oauth-protected-resource/mcp", true)
	auth, err := StartLogin(context.Background(), cfg)
	if err != nil {
		t.Fatalf("starting the login: %v", err)
	}
	defer auth.Close()
	consent(t, auth.URL)
	if err := auth.Wait(context.Background()); err != nil {
		t.Fatalf("completing the login: %v", err)
	}
	if _, _, forms := as.seen(); len(forms) != 1 {
		t.Fatalf("%d token requests, want 1", len(forms))
	}
}

// An expired token is renewed on the way to the server, without a browser.
func TestExpiredTokenRefreshesWithoutAskingAgain(t *testing.T) {
	as, _, cfg := oauthEnv(t, asOptions{}, "/.well-known/oauth-protected-resource/mcp", true)
	auth, err := StartLogin(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	consent(t, auth.URL)
	if err := auth.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}

	// Age the grant the way an hour would.
	store := newTokenStore()
	resource, err := canonicalResource(cfg.URL)
	if err != nil {
		t.Fatal(err)
	}
	rec, ok := store.load(resource)
	if !ok {
		t.Fatal("the login stored nothing")
	}
	rec.ExpiresAt = time.Now().Add(-time.Minute)
	if err := store.save(rec); err != nil {
		t.Fatal(err)
	}

	c, err := Start(context.Background(), cfg)
	if err != nil {
		t.Fatalf("connecting with an expired token: %v", err)
	}
	defer c.Close()

	_, _, forms := as.seen()
	if len(forms) != 2 || forms[1].Get("grant_type") != "refresh_token" {
		t.Fatalf("the client did not refresh: %d token requests", len(forms))
	}
	after, _ := store.load(resource)
	if after.AccessToken == rec.AccessToken {
		t.Error("the refreshed token was not stored")
	}
	if after.RefreshToken == rec.RefreshToken {
		t.Error("the rotated refresh token was not stored; the next refresh will fail")
	}
}

// A grant the server has retired mid-session is replaced on the spot: one 401,
// one refresh, one retry, and the call the user made still succeeds.
func TestARefusedTokenIsReplacedAndTheRequestRetried(t *testing.T) {
	as, _, cfg := oauthEnv(t, asOptions{}, "/.well-known/oauth-protected-resource/mcp", true)
	auth, err := StartLogin(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	consent(t, auth.URL)
	if err := auth.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}

	// A token that has not expired as far as this client knows, but that the
	// server will not accept.
	store := newTokenStore()
	resource, _ := canonicalResource(cfg.URL)
	rec, _ := store.load(resource)
	rec.AccessToken = "retired-token"
	rec.ExpiresAt = time.Now().Add(time.Hour)
	if err := store.save(rec); err != nil {
		t.Fatal(err)
	}

	c, err := Start(context.Background(), cfg)
	if err != nil {
		t.Fatalf("a refused token was not replaced: %v", err)
	}
	defer c.Close()
	if _, _, forms := as.seen(); len(forms) != 2 || forms[1].Get("grant_type") != "refresh_token" {
		t.Fatalf("expected exactly one refresh after the 401, got %d token requests", len(forms))
	}
}

// Nothing to log in with is its own answer, and it names the way out.
func TestConnectingWithoutAGrantAsksForConsent(t *testing.T) {
	_, _, cfg := oauthEnv(t, asOptions{}, "/.well-known/oauth-protected-resource/mcp", true)
	_, err := Start(context.Background(), cfg)
	if err == nil {
		t.Fatal("connected to a server nobody has logged in to")
	}
	if !errors.Is(err, ErrConsentRequired) {
		t.Fatalf("the caller cannot tell this from a broken config: %v", err)
	}
	if !strings.Contains(err.Error(), "/mcp remote") {
		t.Errorf("the error does not say how to fix it: %v", err)
	}
}

// A refused consent says so, and leaves nothing behind to half-configure the
// server with.
func TestRefusedConsentReportsWhyAndStoresNothing(t *testing.T) {
	_, _, cfg := oauthEnv(t, asOptions{denyConsent: true}, "/.well-known/oauth-protected-resource/mcp", true)
	auth, err := StartLogin(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	consent(t, auth.URL)
	err = auth.Wait(context.Background())
	if err == nil {
		t.Fatal("a refused consent was reported as a login")
	}
	if !strings.Contains(err.Error(), "refused") {
		t.Errorf("the error does not say what happened: %v", err)
	}
	if n := storedGrants(t); n != 0 {
		t.Errorf("a refused consent left %d files in the token store", n)
	}
	if _, err := Start(context.Background(), cfg); !errors.Is(err, ErrConsentRequired) {
		t.Errorf("after a refusal the server should still ask for consent, got %v", err)
	}
}

// A redirect that did not come from this login is not a login. Without the
// state check any page open in the browser could drive a code of its choosing
// into the listener and have it exchanged.
func TestARedirectWithTheWrongStateIsRefused(t *testing.T) {
	as, _, cfg := oauthEnv(t, asOptions{}, "/.well-known/oauth-protected-resource/mcp", true)
	auth, err := StartLogin(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	redirect, err := url.Parse(auth.URL)
	if err != nil {
		t.Fatal(err)
	}
	back := redirect.Query().Get("redirect_uri")
	consent(t, back+"?code=injected&state=not-the-state")

	if err := auth.Wait(context.Background()); err == nil {
		t.Fatal("a redirect carrying somebody else's state completed the login")
	}
	if _, _, forms := as.seen(); len(forms) != 0 {
		t.Errorf("the injected code was sent to the token endpoint: %v", forms)
	}
	if n := storedGrants(t); n != 0 {
		t.Errorf("nothing should have been stored, found %d files", n)
	}
}

// The document has to name the issuer it was fetched for. Otherwise a hostile
// resource points at a real authorization server's metadata and collects a code
// minted for somewhere else.
func TestMetadataNamingAnotherIssuerIsRefused(t *testing.T) {
	_, _, cfg := oauthEnv(t, asOptions{issuer: "https://someone-else.example.com"},
		"/.well-known/oauth-protected-resource/mcp", true)
	if _, err := StartLogin(context.Background(), cfg); err == nil {
		t.Fatal("a metadata document for another issuer was accepted")
	}
}

// PKCE with S256 is not negotiable: the code lands on a loopback port any other
// program on this machine can talk to.
func TestAServerWithoutS256IsRefused(t *testing.T) {
	_, _, cfg := oauthEnv(t, asOptions{plainPKCE: true}, "/.well-known/oauth-protected-resource/mcp", true)
	_, err := StartLogin(context.Background(), cfg)
	if err == nil {
		t.Fatal("an authorization server offering only plain PKCE was accepted")
	}
	if !strings.Contains(err.Error(), "PKCE") {
		t.Errorf("the error does not name the reason: %v", err)
	}
}

// The grant is gone: say so as a login rather than as a request failure, and
// do not go on presenting a refresh token that has been refused.
func TestARevokedGrantAsksForConsentAgain(t *testing.T) {
	as, _, cfg := oauthEnv(t, asOptions{refuseRefresh: true}, "/.well-known/oauth-protected-resource/mcp", true)
	store := newTokenStore()
	resource, _ := canonicalResource(cfg.URL)
	if err := store.save(oauthRecord{
		Resource:      resource,
		Issuer:        as.URL(),
		TokenEndpoint: as.URL() + "/token",
		ClientID:      "client-1",
		RedirectURI:   "http://127.0.0.1:1/callback",
		AccessToken:   "stale",
		RefreshToken:  "refresh-gone",
		ExpiresAt:     time.Now().Add(-time.Minute),
	}); err != nil {
		t.Fatal(err)
	}
	_, err := Start(context.Background(), cfg)
	if !errors.Is(err, ErrConsentRequired) {
		t.Fatalf("a revoked grant should ask for a login, got %v", err)
	}
	rec, ok := store.load(resource)
	if !ok {
		t.Fatal("the record was removed rather than emptied")
	}
	if rec.RefreshToken != "" || rec.AccessToken != "" {
		t.Error("dead tokens were kept and will be presented again")
	}
	if rec.ClientID == "" {
		t.Error("the client registration was thrown away with the tokens")
	}
}

// Static-header servers are untouched by any of this, and an entry that has
// both keeps its headers while the token wins the one header it owns.
func TestOAuthDoesNotDisturbConfiguredHeaders(t *testing.T) {
	_, prot, cfg := oauthEnv(t, asOptions{}, "/.well-known/oauth-protected-resource/mcp", true)
	cfg.Headers = map[string]string{
		"X-Tenant":      "acme",
		"Authorization": "Bearer a-token-from-the-config-file",
	}
	auth, err := StartLogin(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	consent(t, auth.URL)
	if err := auth.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}
	c, err := Start(context.Background(), cfg)
	if err != nil {
		t.Fatalf("connecting: %v", err)
	}
	defer c.Close()

	seen := prot.headers()
	if seen.Get("X-Tenant") != "acme" {
		t.Errorf("configured header dropped: %q", seen.Get("X-Tenant"))
	}
	if strings.Contains(seen.Get("Authorization"), "from-the-config-file") {
		t.Errorf("a stale Authorization in the config shadowed the token: %q", seen.Get("Authorization"))
	}
}

// An authorization server reached in the clear would hand the token to anyone
// on the path.
func TestAPlaintextAuthorizationServerIsRefused(t *testing.T) {
	t.Setenv("TAPIOCA_CONFIG_DIR", t.TempDir())
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/.well-known/oauth-protected-resource") {
			writeJSON(w, map[string]any{
				"authorization_servers": []string{"http://auth.example.com"},
			})
			return
		}
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	}))
	defer srv.Close()

	_, err := StartLogin(context.Background(), config.MCPServerConfig{
		Name: "remote", URL: srv.URL + "/mcp", Auth: "oauth",
	})
	if err == nil {
		t.Fatal("a plaintext authorization server was accepted")
	}
	if !strings.Contains(err.Error(), "https") {
		t.Errorf("the error does not name the problem: %v", err)
	}
}

func TestCanonicalResource(t *testing.T) {
	cases := []struct {
		in, want string
		wantErr  bool
	}{
		{in: "https://MCP.Example.COM/mcp", want: "https://mcp.example.com/mcp"},
		{in: "https://mcp.example.com/mcp#frag", want: "https://mcp.example.com/mcp"},
		{in: "https://mcp.example.com/", want: "https://mcp.example.com"},
		{in: "https://mcp.example.com/mcp?tenant=1", want: "https://mcp.example.com/mcp?tenant=1"},
		{in: "http://127.0.0.1:8080/mcp", want: "http://127.0.0.1:8080/mcp"},
		{in: "http://mcp.example.com/mcp", wantErr: true},
		{in: "ftp://mcp.example.com/mcp", wantErr: true},
		{in: "/mcp", wantErr: true},
	}
	for _, c := range cases {
		got, err := canonicalResource(c.in)
		if c.wantErr {
			if err == nil {
				t.Errorf("canonicalResource(%q) = %q, want an error", c.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("canonicalResource(%q): %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("canonicalResource(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestAuthParam(t *testing.T) {
	cases := []struct{ header, want string }{
		{`Bearer resource_metadata="https://a/.well-known/x"`, "https://a/.well-known/x"},
		{`Bearer realm="mcp", resource_metadata="https://a/m", error="x"`, "https://a/m"},
		{`Bearer resource_metadata=https://a/m, realm="mcp"`, "https://a/m"},
		{`Bearer RESOURCE_METADATA="https://a/m"`, "https://a/m"},
		{`Bearer x_resource_metadata="https://evil/m"`, ""},
		{`Bearer realm="mcp"`, ""},
		{"", ""},
		// A header value is bytes the server chose. Folding case with
		// strings.ToLower shortens this one, and every offset after it then
		// lands somewhere else in the original.
		{"Bearer realm=\"İİİİ\", resource_metadata=\"https://a/m\"", "https://a/m"},
	}
	for _, c := range cases {
		if got := authParam(c.header, "resource_metadata"); got != c.want {
			t.Errorf("authParam(%q) = %q, want %q", c.header, got, c.want)
		}
	}
}

// A token in a formatted string is a token in the transcript, the log pane and
// whatever crash report carried it there.
func TestTokensDoNotSurviveFormatting(t *testing.T) {
	rec := oauthRecord{
		Resource:     "https://mcp.example.com/mcp",
		AccessToken:  "at-super-secret",
		RefreshToken: "rt-super-secret",
		ClientSecret: "cs-super-secret",
	}
	var printed []string
	// Every verb one of these would arrive under, including the two that print
	// a struct field by field.
	for _, verb := range []string{"%v", "%s", "%+v", "%#v"} {
		printed = append(printed, fmt.Sprintf(verb, rec), fmt.Sprintf(verb, &rec))
	}
	printed = append(printed, fmt.Errorf("connecting: %v", rec).Error())
	for _, s := range printed {
		for _, secret := range []string{"at-super-secret", "rt-super-secret", "cs-super-secret"} {
			if strings.Contains(s, secret) {
				t.Errorf("a secret reached a formatted string: %q", s)
			}
		}
	}
}

// An error from the token endpoint reaches the screen and the log, so it
// carries the reason and not the body it came in.
func TestTokenEndpointErrorsDoNotEchoTheBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, `{"error":"invalid_grant","error_description":"expired",`+
			`"access_token":"at-leaked-anyway"}`)
	}))
	defer srv.Close()

	_, err := exchange(context.Background(), srv.Client(),
		oauthRecord{Resource: "http://127.0.0.1/mcp", TokenEndpoint: srv.URL, ClientID: "c"},
		url.Values{"grant_type": {"refresh_token"}})
	if err == nil {
		t.Fatal("a 400 from the token endpoint was accepted")
	}
	if strings.Contains(err.Error(), "at-leaked-anyway") {
		t.Errorf("the error carried a token out of the body: %v", err)
	}
	if !strings.Contains(err.Error(), "invalid_grant") || !strings.Contains(err.Error(), "expired") {
		t.Errorf("the error does not say why it failed: %v", err)
	}
}

// A description written by whoever runs the authorization server ends up on a
// terminal.
func TestRemoteTextCannotDriveTheTerminal(t *testing.T) {
	got := remoteText("bad\x1b[2Jthing\nand\ta line")
	if strings.ContainsAny(got, "\x1b\n\t") {
		t.Errorf("remoteText left control characters in %q", got)
	}
	if len([]rune(remoteText(strings.Repeat("é", 500)))) > 201 {
		t.Error("remoteText did not bound the length")
	}
}

// The token file holds a live credential and lives next to the config, so it
// gets the permissions the config file gets.
func TestStoredGrantsAreOwnerOnly(t *testing.T) {
	t.Setenv("TAPIOCA_CONFIG_DIR", t.TempDir())
	store := newTokenStore()
	if err := store.save(oauthRecord{Resource: "https://mcp.example.com/mcp", AccessToken: "at"}); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(store.path("https://mcp.example.com/mcp"))
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("token file mode %o, want 600", perm)
	}
	dir, err := os.Stat(store.dir)
	if err != nil {
		t.Fatal(err)
	}
	if perm := dir.Mode().Perm(); perm != 0o700 {
		t.Errorf("token directory mode %o, want 700", perm)
	}
	// The tokens are not in the config file, which is rewritten by every
	// settings change and pasted into every bug report.
	if _, err := os.Stat(filepath.Join(config.Dir(), "config.toml")); err == nil {
		data, _ := os.ReadFile(filepath.Join(config.Dir(), "config.toml"))
		if strings.Contains(string(data), "at") {
			t.Error("the token reached config.toml")
		}
	}
}

func TestUsesOAuth(t *testing.T) {
	cases := []struct {
		cfg  config.MCPServerConfig
		want bool
	}{
		{config.MCPServerConfig{URL: "https://a/mcp", Auth: "oauth"}, true},
		{config.MCPServerConfig{URL: "https://a/mcp", Auth: "OAuth"}, true},
		{config.MCPServerConfig{URL: "https://a/mcp"}, false},
		{config.MCPServerConfig{URL: "https://a/mcp", Auth: "bearer"}, false},
		{config.MCPServerConfig{Command: "srv", Auth: "oauth"}, false},
	}
	for _, c := range cases {
		if got := UsesOAuth(c.cfg); got != c.want {
			t.Errorf("UsesOAuth(%+v) = %v, want %v", c.cfg, got, c.want)
		}
	}
}

// storedGrants counts the files in the token store for this test's config dir.
func storedGrants(t *testing.T) int {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(config.Dir(), "mcp-oauth"))
	if err != nil {
		return 0
	}
	return len(entries)
}
