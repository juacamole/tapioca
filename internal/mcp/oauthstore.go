package mcp

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"tapioca/internal/config"
)

// Tokens do not go in config.toml. That file is rewritten whenever a setting
// changes, it is what people paste into a bug report, and it is read by every
// tool pointed at a dotfiles directory — none of which should be true of a live
// bearer token. They live beside it instead, one file per server, readable only
// by their owner.

// oauthRecord is everything needed to keep talking to one server: the grant
// itself, and the facts about the authorization server that were discovered to
// obtain it. Caching the endpoint and the client alongside the token is what
// makes a refresh possible without a second round of discovery — a startup that
// re-derives all of it is several requests to a host that may be down, to learn
// what was already known.
type oauthRecord struct {
	Resource      string    `json:"resource"`
	Issuer        string    `json:"issuer,omitempty"`
	TokenEndpoint string    `json:"token_endpoint"`
	ClientID      string    `json:"client_id"`
	ClientSecret  string    `json:"client_secret,omitempty"`
	RedirectURI   string    `json:"redirect_uri,omitempty"`
	Scope         string    `json:"scope,omitempty"`
	AccessToken   string    `json:"access_token,omitempty"`
	RefreshToken  string    `json:"refresh_token,omitempty"`
	ExpiresAt     time.Time `json:"expires_at,omitzero"`
}

// String hides the secrets from fmt. A record reaches a %v by accident — inside
// a wrapped error, in a struct someone printed while debugging — and once it
// has, the token is wherever that string went: the transcript, a session file,
// the log pane. GoString covers the other half: %#v ignores String and prints
// every field, and it is what gets typed while chasing a bug.
func (r oauthRecord) String() string {
	return fmt.Sprintf("oauth grant for %s (redacted)", r.Resource)
}

func (r oauthRecord) GoString() string { return r.String() }

// usable reports whether the access token can still be sent. The skew is not
// politeness: a token that expires between this check and the server reading it
// fails the request that carried it, and clocks here and there differ.
func (r oauthRecord) usable(now time.Time) bool {
	if r.AccessToken == "" {
		return false
	}
	if r.ExpiresAt.IsZero() {
		return true // no expiry given; the 401 path is what retires it
	}
	return now.Add(refreshSkew).Before(r.ExpiresAt)
}

// tokenStore is the on-disk cache, one file per resource.
type tokenStore struct{ dir string }

// newTokenStore puts tokens next to the config, since that is the directory
// already holding credentials and already created with owner-only permissions.
func newTokenStore() *tokenStore {
	return &tokenStore{dir: filepath.Join(config.Dir(), "mcp-oauth")}
}

// path names the file for a resource. The hash is what makes it unique — a URL
// is not a filename, and two servers on one host differ only in their path —
// while the host prefix keeps the directory legible to whoever opens it.
func (s *tokenStore) path(resource string) string {
	sum := sha256.Sum256([]byte(resource))
	name := hex.EncodeToString(sum[:8])
	if u, err := url.Parse(resource); err == nil && u.Host != "" {
		name = sanitize(u.Host) + "-" + name
	}
	return filepath.Join(s.dir, name+".json")
}

func (s *tokenStore) load(resource string) (oauthRecord, bool) {
	data, err := os.ReadFile(s.path(resource))
	if err != nil {
		return oauthRecord{}, false
	}
	var rec oauthRecord
	if err := json.Unmarshal(data, &rec); err != nil {
		return oauthRecord{}, false
	}
	// A file whose contents belong to another resource is not this server's
	// grant, whatever its name says.
	if rec.Resource != resource {
		return oauthRecord{}, false
	}
	return rec, true
}

// save writes the grant. The temporary file is created inside the same
// directory and chmodded before anything is written to it, so the token is
// never briefly world-readable and never lands on a filesystem the rename
// cannot cross.
func (s *tokenStore) save(rec oauthRecord) error {
	if rec.Resource == "" {
		return fmt.Errorf("refusing to store a grant with no resource")
	}
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(s.dir, "token-*.tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp) // a no-op once the rename has moved it
	if err := f.Chmod(0o600); err != nil {
		f.Close()
		return err
	}
	if _, err := f.Write(append(data, '\n')); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, s.path(rec.Resource))
}

// canonicalResource returns the identifier the authorization server will see in
// the resource parameter, and the key this grant is cached under. RFC 8707 asks
// for one spelling of the address so a token minted for one server cannot be
// replayed at another: case in the scheme and host carries no meaning, and a
// fragment never reaches a server at all.
func canonicalResource(raw string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return "", fmt.Errorf("not a valid address: %w", err)
	}
	if u.Host == "" {
		return "", fmt.Errorf("no host in %q", raw)
	}
	switch strings.ToLower(u.Scheme) {
	case "https":
	case "http":
		// The token would cross the network in the clear, and so would every
		// request carrying it. Loopback is the exception because there is no
		// network to cross, and it is where a server under development runs.
		if !isLoopbackHost(u.Hostname()) {
			return "", fmt.Errorf("http would send the token in the clear; %s must be https", u.Host)
		}
	default:
		return "", fmt.Errorf("scheme %q is not http or https", u.Scheme)
	}
	u.Scheme = strings.ToLower(u.Scheme)
	u.Host = strings.ToLower(u.Host)
	u.Fragment = ""
	u.RawFragment = ""
	if u.Path == "/" {
		u.Path = ""
	}
	return u.String(), nil
}

// isLoopbackHost reports whether a host is this machine.
//
// It has to be an address literal or the name reserved for one. A prefix test
// does not do it: "127.example.com" starts with "127." and resolves to whatever
// its owner points it at, so anyone who can name a token endpoint could name one
// that passes the https requirement and still crosses the network in the clear
// — which is the entire thing that requirement is for.
func isLoopbackHost(host string) bool {
	// RFC 6761 reserves localhost for the loopback interface, in both the bare
	// and the fully qualified spelling.
	if strings.EqualFold(strings.TrimSuffix(host, "."), "localhost") {
		return true
	}
	ip, err := netip.ParseAddr(strings.Trim(host, "[]"))
	return err == nil && ip.IsLoopback()
}
