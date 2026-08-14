package mcp

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"time"

	"tapioca/internal/config"
	"tapioca/internal/secretenv"
)

// A hosted MCP server — Linear, Atlassian, Notion, GitHub's own endpoint — has
// no token to paste into a header. The token is minted per user by an
// authorization server the config file has never heard of, which is why a
// static header reaches none of them. This is the flow the MCP specification
// describes: the server names its authorization server, that server hands out a
// client on the spot, and the user consents once in a browser.
//
// Only StartLogin is interactive. Connecting, refreshing and every request are
// not: a token that expires mid-run is renewed silently, and one that cannot be
// renewed fails with ErrConsentRequired rather than throwing a browser window
// at someone in the middle of a tool call. A background flow would also be
// unattended by definition — nobody is looking at the screen to notice the
// consent page — and the login would sit there until it timed out.

// ErrConsentRequired means the server needs a browser login that has not
// happened, or whose grant is gone. It is worth a sentinel because the fix is a
// login rather than a look at the config file, and callers say so.
var ErrConsentRequired = errors.New("authorization required")

const (
	// oauthHTTPTimeout bounds one discovery, registration or token request.
	// These are small JSON round trips; a minute spent on one is a failure that
	// has not been reported yet.
	oauthHTTPTimeout = 30 * time.Second

	// refreshSkew renews before the token actually expires. Not politeness: a
	// token that expires between the check here and the server reading it fails
	// the request that carried it, and the two clocks are not the same clock.
	refreshSkew = 60 * time.Second

	// discoveryTimeout bounds all of discovery and registration together, which
	// is up to half a dozen requests. Any one of them is bounded by the client's
	// own timeout; this is what stops the sequence from adding up to a minute
	// each on a host that is answering slowly.
	discoveryTimeout = 60 * time.Second

	// consentTimeout bounds the browser round trip — long enough to find the
	// window, log in and read the consent screen, short enough that a flow
	// abandoned in a closed tab does not hold a listener for the session.
	consentTimeout = 5 * time.Minute

	// maxMetadataBytes caps a discovery document. These URLs come from a server
	// that has not been trusted yet, and an unbounded read from one is a way to
	// spend all the memory on this machine.
	maxMetadataBytes = 512 << 10
)

// UsesOAuth reports whether an entry logs in rather than carrying a token. The
// rule lives in one place so the transport and the UI cannot disagree about
// which servers need a login.
func UsesOAuth(cfg config.MCPServerConfig) bool {
	return strings.TrimSpace(cfg.URL) != "" && strings.EqualFold(strings.TrimSpace(cfg.Auth), "oauth")
}

// oauthSource hands the transport a usable access token, refreshing when it has
// to. One per connected server.
type oauthSource struct {
	name     string
	resource string
	store    *tokenStore
	hc       *http.Client

	// mu is held across the refresh request on purpose. Requests to one MCP
	// server are few and a refresh is rare, so the cost of serialising them is
	// nothing next to the cost of getting it wrong: two goroutines refreshing
	// the same grant at once means one of them presents a refresh token the
	// server has already rotated away, and that burns the grant for both.
	mu  sync.Mutex
	rec oauthRecord
}

func newOAuthSource(cfg config.MCPServerConfig) (*oauthSource, error) {
	resource, err := canonicalResource(cfg.URL)
	if err != nil {
		return nil, fmt.Errorf("mcp %s: %w", cfg.Name, err)
	}
	store := newTokenStore()
	rec, _ := store.load(resource)
	rec.Resource = resource
	return &oauthSource{
		name:     cfg.Name,
		resource: resource,
		store:    store,
		hc:       &http.Client{Timeout: oauthHTTPTimeout},
		rec:      rec,
	}, nil
}

// consentErr names the server, since the fix is a command with its name in it.
func (s *oauthSource) consentErr() error {
	return fmt.Errorf("%w — run /mcp %s to log in", ErrConsentRequired, s.name)
}

// token returns an access token to send, refreshing an expired one.
func (s *oauthSource) token(ctx context.Context) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.rec.usable(time.Now()) {
		return s.rec.AccessToken, nil
	}
	// Another Tapioca on this machine may have refreshed the same grant since
	// this one read it. Rotation makes that consequential: the copy in memory is
	// then a token the server has already retired, and presenting it invalidates
	// the grant for both processes.
	if fresh, ok := s.store.load(s.resource); ok && fresh.RefreshToken != s.rec.RefreshToken {
		s.rec = fresh
		if s.rec.usable(time.Now()) {
			return s.rec.AccessToken, nil
		}
	}
	if s.rec.RefreshToken == "" || s.rec.TokenEndpoint == "" {
		return "", s.consentErr()
	}
	ctx, cancel := context.WithTimeout(ctx, oauthHTTPTimeout)
	defer cancel()
	form := url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {s.rec.RefreshToken},
	}
	if s.rec.Scope != "" {
		form.Set("scope", s.rec.Scope)
	}
	rec, err := exchange(ctx, s.hc, s.rec, form)
	if err != nil {
		// The grant is gone: revoked, past its refresh window, or rotated out
		// from under us. Keeping dead tokens would fail every later request the
		// same way and hide the one thing worth saying. The client registration
		// stays — it is still valid, and registering again on every login is
		// what an authorization server would rather we did not do.
		s.rec.AccessToken, s.rec.RefreshToken, s.rec.ExpiresAt = "", "", time.Time{}
		_ = s.store.save(s.rec)
		return "", fmt.Errorf("%w (the stored grant no longer works: %v)", s.consentErr(), err)
	}
	s.rec = rec
	// A token that works but could not be cached is still a token that works.
	// The cost of the failed write is a login next start, not this request.
	_ = s.store.save(rec)
	return rec.AccessToken, nil
}

// invalidate retires a token the server has just refused. It matches on the
// value so a token another goroutine has already replaced is not thrown away:
// two requests racing the same 401 would otherwise discard the good token the
// first refresh had just obtained.
func (s *oauthSource) invalidate(tok string) {
	if tok == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.rec.AccessToken == tok {
		s.rec.AccessToken, s.rec.ExpiresAt = "", time.Time{}
	}
}

// resourceMeta is RFC 9728: what the MCP server says about who authorizes it.
type resourceMeta struct {
	Resource             string   `json:"resource"`
	AuthorizationServers []string `json:"authorization_servers"`
	ScopesSupported      []string `json:"scopes_supported"`
}

// serverMeta is RFC 8414: where the authorization server's endpoints are.
type serverMeta struct {
	Issuer                string   `json:"issuer"`
	AuthorizationEndpoint string   `json:"authorization_endpoint"`
	TokenEndpoint         string   `json:"token_endpoint"`
	RegistrationEndpoint  string   `json:"registration_endpoint"`
	ScopesSupported       []string `json:"scopes_supported"`
	CodeChallengeMethods  []string `json:"code_challenge_methods_supported"`
}

// discoverResource finds the protected-resource document. The pointer in the
// server's own 401 is authoritative and is tried first; the well-known paths
// are a guess that happens to be right most of the time, and a server behind a
// gateway that rewrites paths is only reachable through the header.
func discoverResource(ctx context.Context, hc *http.Client, resource string) (resourceMeta, bool) {
	var candidates []string
	if u := challengeMetadata(ctx, hc, resource); u != "" {
		candidates = append(candidates, u)
	}
	candidates = append(candidates, wellKnownResourceURLs(resource)...)
	for _, c := range candidates {
		var rm resourceMeta
		if err := fetchJSON(ctx, hc, c, &rm); err == nil && len(rm.AuthorizationServers) > 0 {
			return rm, true
		}
	}
	return resourceMeta{}, false
}

// wellKnownResourceURLs lists where a resource's metadata may live, most
// specific first. RFC 9728 puts the resource's own path after the well-known
// segment; servers mounted at a path routinely publish only the root document.
func wellKnownResourceURLs(resource string) []string {
	u, err := url.Parse(resource)
	if err != nil {
		return nil
	}
	root := u.Scheme + "://" + u.Host
	path := strings.TrimSuffix(u.Path, "/")
	var out []string
	if path != "" {
		out = append(out, root+"/.well-known/oauth-protected-resource"+path)
	}
	return append(out, root+"/.well-known/oauth-protected-resource")
}

// wellKnownServerURLs lists where an authorization server's metadata may live.
// The order is the specification's: the OAuth document before the OpenID one,
// path-inserted before path-suffixed, because an issuer with a path is a tenant
// and the tenant's document is the one that describes it.
func wellKnownServerURLs(issuer string) []string {
	u, err := url.Parse(issuer)
	if err != nil {
		return nil
	}
	root := u.Scheme + "://" + u.Host
	path := strings.TrimSuffix(u.Path, "/")
	if path == "" {
		return []string{
			root + "/.well-known/oauth-authorization-server",
			root + "/.well-known/openid-configuration",
		}
	}
	return []string{
		root + "/.well-known/oauth-authorization-server" + path,
		root + "/.well-known/openid-configuration" + path,
		root + path + "/.well-known/openid-configuration",
	}
}

// discoverServer reads the authorization server's metadata, falling back to the
// endpoints in their default places for a server that publishes none — which
// the MCP specification allows, and several servers rely on.
func discoverServer(ctx context.Context, hc *http.Client, issuer string) (serverMeta, error) {
	for _, c := range wellKnownServerURLs(issuer) {
		var sm serverMeta
		if err := fetchJSON(ctx, hc, c, &sm); err != nil {
			continue
		}
		// RFC 8414: the document must name the issuer it was fetched for.
		// Otherwise a hostile resource can point at a real authorization
		// server's metadata and collect a code minted for somewhere else.
		if sm.Issuer != "" && strings.TrimSuffix(sm.Issuer, "/") != strings.TrimSuffix(issuer, "/") {
			return serverMeta{}, fmt.Errorf("%s claims to be %s; refusing to send you there", c, sm.Issuer)
		}
		if sm.AuthorizationEndpoint != "" && sm.TokenEndpoint != "" {
			return sm, nil
		}
	}
	root := strings.TrimSuffix(issuer, "/")
	return serverMeta{
		Issuer:                issuer,
		AuthorizationEndpoint: root + "/authorize",
		TokenEndpoint:         root + "/token",
		RegistrationEndpoint:  root + "/register",
	}, nil
}

// challengeMetadata asks the server itself where its metadata lives.
//
// The probe is a real initialize request rather than an empty body: a server
// that validates before it authenticates answers nonsense with 400, and a 400
// carries no challenge to read.
func challengeMetadata(ctx context.Context, hc *http.Client, resource string) string {
	body := `{"jsonrpc":"2.0","id":0,"method":"initialize","params":{"protocolVersion":"` +
		protocolVersion + `","capabilities":{},"clientInfo":{"name":"tapioca","version":"0"}}}`
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, resource, strings.NewReader(body))
	if err != nil {
		return ""
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	resp, err := hc.Do(req)
	if err != nil {
		return ""
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		return ""
	}
	return authParam(resp.Header.Get("WWW-Authenticate"), "resource_metadata")
}

// authParam pulls one parameter out of a WWW-Authenticate header. A general
// parser for that header is more than is needed and more than can be guessed
// at safely; the only parameter that matters here is the pointer to the
// metadata document.
func authParam(header, name string) string {
	low, lowName := asciiLower(header), asciiLower(name)
	for i := 0; i < len(low); {
		j := strings.Index(low[i:], lowName)
		if j < 0 {
			return ""
		}
		pos := i + j
		i = pos + 1
		// A name must start a parameter, so "x_resource_metadata" is not one.
		if pos > 0 && !strings.ContainsRune(" ,;", rune(header[pos-1])) {
			continue
		}
		rest := strings.TrimLeft(header[pos+len(name):], " ")
		if !strings.HasPrefix(rest, "=") {
			continue
		}
		rest = strings.TrimLeft(rest[1:], " ")
		if after, ok := strings.CutPrefix(rest, `"`); ok {
			if end := strings.Index(after, `"`); end >= 0 {
				return after[:end]
			}
			return ""
		}
		if end := strings.IndexAny(rest, ", "); end >= 0 {
			return rest[:end]
		}
		return rest
	}
	return ""
}

// asciiLower folds case without moving a single byte, which strings.ToLower
// does not: a header value carrying İ comes back a byte shorter, and every
// offset found in the folded copy then points somewhere else in the original —
// at best the wrong parameter's value, at worst past the end of it. Parameter
// names are ASCII, so nothing is lost by folding only that.
func asciiLower(s string) string {
	b := []byte(s)
	for i, c := range b {
		if c >= 'A' && c <= 'Z' {
			b[i] = c + ('a' - 'A')
		}
	}
	return string(b)
}

// fetchJSON reads one metadata document from a host that has not been trusted
// yet: the address is checked before the request and the body is bounded.
func fetchJSON(ctx context.Context, hc *http.Client, raw string, into any) error {
	if err := checkOAuthURL(raw); err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, raw, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("MCP-Protocol-Version", protocolVersion)
	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s: %s", raw, resp.Status)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxMetadataBytes))
	if err != nil {
		return err
	}
	return json.Unmarshal(body, into)
}

// checkOAuthURL refuses an address that would carry the flow, or the token,
// somewhere it must not go. Everything here comes out of a document fetched
// from the network, so none of it is trusted for being written down.
func checkOAuthURL(raw string) error {
	_, err := canonicalResource(raw)
	return err
}

// register asks the authorization server for a client of our own. Most hosted
// MCP servers have no page to get a client id from and no expectation that
// anyone has one: RFC 7591 is how the specification expects a client nobody has
// heard of to obtain credentials.
func register(ctx context.Context, hc *http.Client, endpoint, redirectURI, scope string) (id, secret string, err error) {
	if endpoint == "" {
		return "", "", fmt.Errorf("this authorization server does not register clients; set client_id in the [[mcp]] entry")
	}
	if err := checkOAuthURL(endpoint); err != nil {
		return "", "", err
	}
	body := map[string]any{
		"client_name":                "Tapioca",
		"client_uri":                 "https://github.com/juacamole/tapioca",
		"redirect_uris":              []string{redirectURI},
		"grant_types":                []string{"authorization_code", "refresh_token"},
		"response_types":             []string{"code"},
		"application_type":           "native",
		"token_endpoint_auth_method": "none",
	}
	if scope != "" {
		body["scope"] = scope
	}
	data, err := json.Marshal(body)
	if err != nil {
		return "", "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(string(data)))
	if err != nil {
		return "", "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := hc.Do(req)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return "", "", oauthError("registering a client", resp)
	}
	var out struct {
		ClientID     string `json:"client_id"`
		ClientSecret string `json:"client_secret"`
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxMetadataBytes))
	if err != nil {
		return "", "", err
	}
	if err := json.Unmarshal(raw, &out); err != nil || out.ClientID == "" {
		return "", "", fmt.Errorf("registering a client: the answer carried no client_id")
	}
	return out.ClientID, out.ClientSecret, nil
}

// exchange posts a grant to the token endpoint and folds the answer into the
// record. The two grants this client uses differ in three form fields, and what
// has to be done with the answer — a rotated refresh token, an absent expiry, a
// scope the server narrowed — is the same for both.
func exchange(ctx context.Context, hc *http.Client, rec oauthRecord, form url.Values) (oauthRecord, error) {
	if err := checkOAuthURL(rec.TokenEndpoint); err != nil {
		return rec, err
	}
	form.Set("client_id", rec.ClientID)
	if rec.ClientSecret != "" {
		form.Set("client_secret", rec.ClientSecret)
	}
	// RFC 8707. The audience is what stops a token minted for this server being
	// spent at another one that shares an authorization server.
	form.Set("resource", rec.Resource)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, rec.TokenEndpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return rec, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := hc.Do(req)
	if err != nil {
		return rec, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return rec, oauthError("the token endpoint refused", resp)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxMetadataBytes))
	if err != nil {
		return rec, err
	}
	var out struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		TokenType    string `json:"token_type"`
		ExpiresIn    int64  `json:"expires_in"`
		Scope        string `json:"scope"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return rec, fmt.Errorf("the token endpoint did not answer with json")
	}
	if out.AccessToken == "" {
		return rec, fmt.Errorf("the token endpoint returned no access token")
	}
	// Anything other than a bearer token needs a proof of possession this client
	// cannot produce, and sending it as a bearer would fail at the far end with
	// someone else's error message.
	if out.TokenType != "" && !strings.EqualFold(out.TokenType, "bearer") {
		return rec, fmt.Errorf("the token is a %s token, which this client cannot present", remoteText(out.TokenType))
	}
	rec.AccessToken = out.AccessToken
	if out.RefreshToken != "" {
		// Rotation: a server that issues a new refresh token has retired the old
		// one, and keeping it means the next refresh fails.
		rec.RefreshToken = out.RefreshToken
	}
	rec.ExpiresAt = time.Time{}
	if out.ExpiresIn > 0 {
		rec.ExpiresAt = time.Now().Add(time.Duration(out.ExpiresIn) * time.Second)
	}
	if out.Scope != "" {
		rec.Scope = out.Scope
	}
	return rec, nil
}

// oauthError reports why a request failed without echoing the body. A token
// endpoint's answer can hold a token even when it also holds an error, and an
// error string ends up on the screen and in the log — which is the one place a
// token must never reach.
func oauthError(op string, resp *http.Response) error {
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, maxMetadataBytes))
	var out struct {
		Error       string `json:"error"`
		Description string `json:"error_description"`
	}
	if json.Unmarshal(raw, &out) == nil && out.Error != "" {
		if out.Description != "" {
			return fmt.Errorf("%s: %s (%s)", op, remoteText(out.Error), remoteText(out.Description))
		}
		return fmt.Errorf("%s: %s", op, remoteText(out.Error))
	}
	return fmt.Errorf("%s: %s", op, resp.Status)
}

// remoteText makes text written by someone else safe to put on a terminal. An
// error line is not a place to let another party's escape sequences through,
// and a status line is not a place for a paragraph.
func remoteText(s string) string {
	s = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return ' '
		}
		return r
	}, s)
	s = strings.TrimSpace(s)
	if r := []rune(s); len(r) > 200 {
		return string(r[:200]) + "…"
	}
	return s
}

// Authorization is a login waiting on a browser. It is handed back before the
// consent so the caller can show the URL: on a machine where no browser opens —
// a box reached over ssh is the ordinary case — that URL is the only way in,
// and a flow that has already shut its listener cannot be finished by hand.
type Authorization struct {
	URL    string // where the user consents
	Server string // the [[mcp]] entry this login is for

	rec      oauthRecord
	verifier string
	state    string
	store    *tokenStore
	hc       *http.Client

	ln        net.Listener
	result    chan authResult
	closeOnce sync.Once
}

type authResult struct {
	code string
	err  error
}

// StartLogin discovers the authorization server, obtains a client and opens the
// loopback listener the redirect will land on. Nothing is written to disk here:
// a consent that is refused or abandoned must leave no half-configured server
// behind, and until the code has been exchanged there is nothing worth keeping.
func StartLogin(ctx context.Context, cfg config.MCPServerConfig) (*Authorization, error) {
	if !UsesOAuth(cfg) {
		return nil, fmt.Errorf("mcp %s: this entry is not auth = \"oauth\"", cfg.Name)
	}
	resource, err := canonicalResource(cfg.URL)
	if err != nil {
		return nil, fmt.Errorf("mcp %s: %w", cfg.Name, err)
	}
	hc := &http.Client{Timeout: oauthHTTPTimeout}
	store := newTokenStore()
	rec, _ := store.load(resource) // reuse an earlier client registration
	rec.Resource = resource

	setup, cancel := context.WithTimeout(ctx, discoveryTimeout)
	defer cancel()

	rm, _ := discoverResource(setup, hc, resource)
	issuer := ""
	if len(rm.AuthorizationServers) > 0 {
		issuer = strings.TrimSpace(rm.AuthorizationServers[0])
	}
	if issuer == "" {
		// No metadata document. A server that authorizes itself is common
		// enough — and the alternative is refusing to try at all.
		u, _ := url.Parse(resource)
		issuer = u.Scheme + "://" + u.Host
	}
	if err := checkOAuthURL(issuer); err != nil {
		return nil, fmt.Errorf("mcp %s: authorization server %s: %w", cfg.Name, issuer, err)
	}
	sm, err := discoverServer(setup, hc, issuer)
	if err != nil {
		return nil, fmt.Errorf("mcp %s: %w", cfg.Name, err)
	}
	if err := checkOAuthURL(sm.AuthorizationEndpoint); err != nil {
		return nil, fmt.Errorf("mcp %s: authorization endpoint: %w", cfg.Name, err)
	}
	// PKCE is not optional here. A server that lists its methods and omits S256
	// is offering plain, which protects nothing on a loopback redirect that any
	// other program on this machine can read.
	if len(sm.CodeChallengeMethods) > 0 && !contains(sm.CodeChallengeMethods, "S256") {
		return nil, fmt.Errorf("mcp %s: the authorization server does not support PKCE with S256", cfg.Name)
	}

	ln, redirect, err := listenForRedirect(rec.RedirectURI)
	if err != nil {
		return nil, fmt.Errorf("mcp %s: %w", cfg.Name, err)
	}
	scope := strings.Join(cfg.Scopes, " ")
	if scope == "" {
		// What the resource asks for, not what the authorization server offers:
		// an authorization server advertises every scope any of its clients may
		// want, and asking for all of them is a consent screen demanding far
		// more than this connection needs.
		scope = strings.Join(rm.ScopesSupported, " ")
	}

	// A metadata document need not repeat its own issuer, so the one that was
	// asked about stands in. Compared before it is written back, or a stored
	// client would never look like it matched.
	issuerID := sm.Issuer
	if issuerID == "" {
		issuerID = issuer
	}
	knownClient := rec.ClientID != "" && rec.RedirectURI == redirect && rec.Issuer == issuerID
	rec.Issuer, rec.TokenEndpoint = issuerID, sm.TokenEndpoint
	switch {
	case strings.TrimSpace(cfg.ClientID) != "":
		rec.ClientID, rec.ClientSecret = strings.TrimSpace(cfg.ClientID), ""
	case !knownClient:
		id, secret, err := register(setup, hc, sm.RegistrationEndpoint, redirect, scope)
		if err != nil {
			ln.Close()
			return nil, fmt.Errorf("mcp %s: %w", cfg.Name, err)
		}
		rec.ClientID, rec.ClientSecret = id, secret
	}
	rec.RedirectURI, rec.Scope = redirect, scope

	verifier, err := randomToken()
	if err != nil {
		ln.Close()
		return nil, err
	}
	state, err := randomToken()
	if err != nil {
		ln.Close()
		return nil, err
	}

	au, err := url.Parse(sm.AuthorizationEndpoint)
	if err != nil {
		ln.Close()
		return nil, fmt.Errorf("mcp %s: authorization endpoint: %w", cfg.Name, err)
	}
	// Merged rather than replaced: an authorization endpoint is allowed to carry
	// query parameters of its own, and dropping them breaks tenanted servers.
	q := au.Query()
	q.Set("response_type", "code")
	q.Set("client_id", rec.ClientID)
	q.Set("redirect_uri", redirect)
	q.Set("state", state)
	q.Set("code_challenge", pkceChallenge(verifier))
	q.Set("code_challenge_method", "S256")
	q.Set("resource", resource)
	if scope != "" {
		q.Set("scope", scope)
	}
	au.RawQuery = q.Encode()

	a := &Authorization{
		URL:      au.String(),
		Server:   cfg.Name,
		rec:      rec,
		verifier: verifier,
		state:    state,
		store:    store,
		hc:       hc,
		ln:       ln,
		result:   make(chan authResult, 1),
	}
	srv := &http.Server{Handler: http.HandlerFunc(a.handle), ReadHeaderTimeout: 10 * time.Second}
	// Without this the browser holds the connection open after the redirect and
	// the serving goroutine outlives the flow that started it.
	srv.SetKeepAlivesEnabled(false)
	go func() { _ = srv.Serve(ln) }() // returns when Close shuts the listener
	return a, nil
}

// listenForRedirect binds the loopback listener the redirect comes back to.
//
// The port from the last successful login is asked for first because a
// registered client is tied to the exact redirect URI it registered, port and
// all. RFC 8252 says an authorization server must accept any loopback port for
// a native client; enough of them do not that relying on it produces a login
// that fails for no visible reason. When the old port is taken, a fresh one is
// used and the client is registered again against it.
func listenForRedirect(previous string) (net.Listener, string, error) {
	if previous != "" {
		if u, err := url.Parse(previous); err == nil && u.Port() != "" {
			if ln, err := net.Listen("tcp", "127.0.0.1:"+u.Port()); err == nil {
				return ln, previous, nil
			}
		}
	}
	// 127.0.0.1 rather than a wildcard: the code lands here in the clear, and
	// nothing outside this machine has any business reaching the listener.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, "", fmt.Errorf("opening a local listener for the login: %w", err)
	}
	addr, ok := ln.Addr().(*net.TCPAddr)
	if !ok {
		ln.Close()
		return nil, "", fmt.Errorf("opening a local listener for the login: not a tcp address")
	}
	return ln, fmt.Sprintf("http://127.0.0.1:%d/callback", addr.Port), nil
}

func (a *Authorization) handle(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/callback" {
		http.NotFound(w, r)
		return
	}
	q := r.URL.Query()
	switch {
	case q.Get("state") != a.state:
		// The state proves the redirect belongs to the flow that started here.
		// Without the check, any page open in the browser could drive a code of
		// its own choosing into this listener and have it exchanged.
		writePage(w, "This did not come from Tapioca. Nothing was authorized.")
		a.finish(authResult{err: fmt.Errorf("the redirect did not carry this login's state")})
	case q.Get("error") != "":
		writePage(w, "Authorization was refused. You can close this tab.")
		a.finish(authResult{err: consentError(q.Get("error"), q.Get("error_description"))})
	case q.Get("code") == "":
		writePage(w, "The authorization server sent no code. You can close this tab.")
		a.finish(authResult{err: fmt.Errorf("the redirect carried no authorization code")})
	default:
		writePage(w, "Tapioca is authorized. You can close this tab.")
		a.finish(authResult{code: q.Get("code")})
	}
}

// writePage answers the browser and flushes before the caller closes the
// listener. Nothing from the query string reaches the page: error_description
// is written by whoever runs the authorization server, and a page served from
// 127.0.0.1 is one the browser trusts more than most.
func writePage(w http.ResponseWriter, msg string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprintf(w, "<!doctype html><meta charset=utf-8><title>Tapioca</title>"+
		"<body style=\"font:16px system-ui;padding:3rem\"><p>%s</p>", msg)
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}

// finish records the outcome without blocking. The channel holds one result and
// the browser may hit the listener more than once — a reload, a favicon request
// answered with a 404, a second tab — and a handler that blocked on a full
// channel would hold the connection open forever.
func (a *Authorization) finish(res authResult) {
	select {
	case a.result <- res:
	default:
	}
}

// consentError names what the authorization server refused. access_denied is
// the ordinary case — someone pressed the other button — and reporting that as
// a failure sends people looking for a bug that is not there.
func consentError(code, desc string) error {
	if code == "access_denied" {
		return fmt.Errorf("consent was refused")
	}
	if desc != "" {
		return fmt.Errorf("the authorization server refused: %s (%s)", remoteText(code), remoteText(desc))
	}
	return fmt.Errorf("the authorization server refused: %s", remoteText(code))
}

// Wait blocks for the consent, exchanges the code and stores the grant. The
// server is usable as soon as it returns.
func (a *Authorization) Wait(ctx context.Context) error {
	defer a.Close()
	// The consent and the exchange get separate budgets. Sharing one would give
	// a login completed at the last minute no time left to spend its code.
	consentCtx, cancel := context.WithTimeout(ctx, consentTimeout)
	defer cancel()
	var res authResult
	select {
	case res = <-a.result:
	case <-consentCtx.Done():
		return fmt.Errorf("mcp %s: the browser login did not finish: %w", a.Server, consentCtx.Err())
	}
	if res.err != nil {
		return fmt.Errorf("mcp %s: %w", a.Server, res.err)
	}
	form := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {res.code},
		"redirect_uri":  {a.rec.RedirectURI},
		"code_verifier": {a.verifier},
	}
	exCtx, exCancel := context.WithTimeout(ctx, oauthHTTPTimeout)
	defer exCancel()
	rec, err := exchange(exCtx, a.hc, a.rec, form)
	if err != nil {
		return fmt.Errorf("mcp %s: %w", a.Server, err)
	}
	if err := a.store.save(rec); err != nil {
		return fmt.Errorf("mcp %s: storing the grant: %w", a.Server, err)
	}
	return nil
}

// Close drops the listener. Only the listener: a response still on its way to
// the browser is written by a connection this does not touch, and cutting it
// off leaves the user looking at a failed page after a login that worked.
func (a *Authorization) Close() {
	a.closeOnce.Do(func() { _ = a.ln.Close() })
}

// randomToken returns a URL-safe random string. crypto/rand only: the state and
// the verifier are the two things standing between this flow and a code
// injected by whoever can guess them.
func randomToken() (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("no randomness available for the login: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b[:]), nil
}

func pkceChallenge(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

func contains(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}

// OpenBrowser asks the desktop to open a URL. Failure is never fatal: every
// caller still holds the URL to show, and a machine with no browser — a server
// over ssh — is an ordinary place to be running this.
func OpenBrowser(target string) error {
	// The address comes out of a discovery document, so it is checked before it
	// is handed to a program that will act on it. A file:// or javascript: URL
	// reaching the desktop opener is not a browser tab.
	u, err := url.Parse(target)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") {
		return fmt.Errorf("refusing to open %q", target)
	}
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", target)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", target)
	default:
		cmd = exec.Command("xdg-open", target)
	}
	// A browser started from here inherits this environment, and the provider
	// keys in it are worth more than anything it is being opened for.
	cmd.Env = secretenv.Scrubbed()
	return cmd.Start()
}
