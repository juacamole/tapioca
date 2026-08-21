package httpsafe

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
)

func check(t *testing.T, from, to string, hops int) error {
	t.Helper()
	fu, err := url.Parse(from)
	if err != nil {
		t.Fatal(err)
	}
	tu, err := url.Parse(to)
	if err != nil {
		t.Fatal(err)
	}
	via := make([]*http.Request, 0, hops)
	for i := 0; i < hops; i++ {
		via = append(via, &http.Request{URL: fu})
	}
	return SameOrigin(&http.Request{URL: tu}, via)
}

func TestSameOriginAllowsWhatItShould(t *testing.T) {
	for _, tc := range []struct{ from, to string }{
		// The ordinary reasons an API redirects at all.
		{"https://api.example.com/v1", "https://api.example.com/v1/"},
		{"https://api.example.com/v1/models", "https://api.example.com/v2/models"},
		{"http://127.0.0.1:8080/v1", "http://127.0.0.1:8080/v1/chat"},
		// An upgrade to TLS on the same host only ever narrows what the hop
		// exposes.
		{"http://api.example.com/v1", "https://api.example.com/v1"},
		// Case in a host name is not a difference.
		{"https://API.example.com/v1", "https://api.EXAMPLE.com/v1"},
	} {
		if err := check(t, tc.from, tc.to, 1); err != nil {
			t.Errorf("%s -> %s was refused: %v", tc.from, tc.to, err)
		}
	}
}

func TestSameOriginRefusesWhatItShould(t *testing.T) {
	for _, tc := range []struct{ from, to, why string }{
		{"https://api.example.com/v1", "https://evil.example.net/v1", "another host"},
		{"https://api.example.com/v1", "https://api.example.com.evil.net/v1", "a suffix that only looks like the host"},
		{"https://api.example.com/v1", "https://sub.api.example.com/v1", "a subdomain is a different host"},
		{"https://api.example.com/v1", "http://api.example.com/v1", "a downgrade to plaintext"},
		{"http://127.0.0.1:8080/v1", "http://127.0.0.1:9090/v1", "another port on the same address"},
		{"https://api.example.com/v1", "http://169.254.169.254/latest/meta-data/", "the cloud metadata endpoint"},
	} {
		err := check(t, tc.from, tc.to, 1)
		if err == nil {
			t.Errorf("%s: %s -> %s was allowed", tc.why, tc.from, tc.to)
			continue
		}
		// The message must not carry the URL: with auth_style = "query" the
		// credential is in it, and this error is shown on screen.
		if strings.Contains(err.Error(), "/v1") || strings.Contains(err.Error(), "meta-data") {
			t.Errorf("the refusal quotes the path: %v", err)
		}
	}
}

func TestSameOriginStopsAChain(t *testing.T) {
	if err := check(t, "https://a.example.com/", "https://a.example.com/x", maxHops); err == nil {
		t.Error("a chain of maxHops redirects was allowed to continue")
	}
}
