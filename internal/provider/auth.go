package provider

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"tapioca/internal/config"
)

// Every provider Tapioca ships knows where its own credential goes. A custom
// one does not: the same OpenAI wire format is served behind an Authorization
// bearer, an X-API-Key header, a query parameter, and nothing at all. So for
// those the placement is configured rather than coded.

// Auth styles.
const (
	AuthBearer = "bearer" // Authorization: Bearer <key> — the default
	AuthHeader = "header" // a named header carries the bare key
	AuthQuery  = "query"  // a query parameter carries the key
	AuthNone   = "none"   // no credential, for a local server
)

// AuthStyles lists the choices in the order the UI offers them.
var AuthStyles = []struct{ Name, Desc string }{
	{AuthBearer, "Authorization: Bearer <key> (most providers)"},
	{AuthHeader, "a named header holds the key"},
	{AuthQuery, "a query parameter holds the key"},
	{AuthNone, "no credential — a local server"},
}

// authSpec is how one provider's credential is presented.
type authSpec struct {
	style  string
	header string // for AuthHeader
	query  string // for AuthQuery
	key    string
	extra  map[string]string
}

// apply puts the credential where the style says, and the extra headers with
// it. Extra headers are applied first so a misconfigured one cannot overwrite
// the credential — a header named "Authorization" in that map would otherwise
// silently replace the real one and send the request unauthenticated, or
// authenticated as something else.
func (a authSpec) apply(r *http.Request) error {
	for name, value := range a.extra {
		if err := validHeader(name, value); err != nil {
			return err
		}
		if strings.EqualFold(name, "Authorization") {
			return fmt.Errorf("header %q would replace the credential; set auth_style instead", name)
		}
		r.Header.Set(name, value)
	}

	switch a.style {
	case AuthNone:
		return nil
	case AuthHeader:
		if a.header == "" {
			return fmt.Errorf("auth_style is %q but auth_header is not set", AuthHeader)
		}
		if err := validHeader(a.header, a.key); err != nil {
			return err
		}
		r.Header.Set(a.header, a.key)
	case AuthQuery:
		if a.query == "" {
			return fmt.Errorf("auth_style is %q but auth_query is not set", AuthQuery)
		}
		q := r.URL.Query()
		q.Set(a.query, a.key)
		r.URL.RawQuery = q.Encode()
	default:
		r.Header.Set("Authorization", "Bearer "+a.key)
	}
	return nil
}

// validHeader rejects names and values that would let a config entry inject a
// second header or terminate the request line.
func validHeader(name, value string) error {
	if name == "" {
		return fmt.Errorf("a header name is empty")
	}
	if strings.ContainsAny(name, "\r\n: ") {
		return fmt.Errorf("header name %q contains a separator", name)
	}
	if strings.ContainsAny(value, "\r\n") {
		return fmt.Errorf("the value of header %q contains a newline", name)
	}
	return nil
}

// CheckBaseURL rejects an address that would send conversations somewhere
// unintended. Everything typed into this app goes to that host, so a typo
// deserves a refusal rather than a silent plaintext request across a network.
func CheckBaseURL(raw string) error {
	if strings.TrimSpace(raw) == "" {
		return fmt.Errorf("an address is required")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("not a valid address: %w", err)
	}
	if u.Host == "" {
		return fmt.Errorf("no host in %q — it needs a scheme, like https://api.example.com/v1", raw)
	}
	switch u.Scheme {
	case "https":
		return nil
	case "http":
		if isLoopback(u.Hostname()) {
			return nil
		}
		return fmt.Errorf("http would send prompts across the network in the clear; use https (http is allowed for localhost)")
	default:
		return fmt.Errorf("scheme %q is not http or https", u.Scheme)
	}
}

// isLoopback reports whether a host is this machine. It has to be an address
// literal or the name reserved for one. A prefix test does not do it:
// "127.example.com" starts with "127." and resolves to wherever its owner
// points it, so a base_url anyone could suggest passed the https requirement
// and still sent every prompt, and the API key, across the network in the
// clear — which is the entire thing that requirement is for.
//
// The test itself lives in the config package, which asks the same question of
// a base_url a repository wrote. Two copies of a predicate this load-bearing
// would eventually answer differently.
func isLoopback(host string) bool { return config.IsLoopbackHost(host) }
