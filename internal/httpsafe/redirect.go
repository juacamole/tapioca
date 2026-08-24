// Package httpsafe holds the redirect policy for the HTTP clients that carry a
// credential: the provider clients and the MCP HTTP transport.
//
// It is one package rather than a copy in each because "may this redirect carry
// my credential" is one question, and two answers to it is how the half nobody
// looked at ends up being the one that runs.
package httpsafe

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// maxHops bounds a redirect chain. net/http's own default is ten; a credentialed
// API call that needs more than a couple is not a call anyone configured.
const maxHops = 5

// SameOrigin is a CheckRedirect that keeps a credentialed request on the origin
// it was aimed at.
//
// net/http's default follows ten hops to anywhere and strips only Authorization,
// Www-Authenticate, Cookie and Cookie2 along the way. That default is exactly
// what makes the gap easy to miss, because every credential this program puts
// somewhere else survives it: x-api-key for Anthropic, api-key for Azure,
// whatever header a custom gateway is configured with, X-Amz-Security-Token for
// Bedrock, and the configured headers of an MCP server. So does the request
// body, which for a turn is the system prompt and the whole conversation, and
// which net/http replays on a 307 or 308 because the request was built from a
// reader that can be rewound.
//
// The endpoint is one of the untrusted inputs — a local model server, a gateway
// someone was told to point at, an MCP server named by a config found in the
// tree — and a redirect is a reply it is allowed to send. One 302 and the key
// and the conversation are at a host nobody named, over a scheme CheckBaseURL
// exists to refuse. CheckBaseURL only ever sees the string that was configured;
// the hop that gets around it is the one that was not.
//
// tools/web.go answered the same question the same way for the fetch tool, and
// for the same reason: the destination is what the user approved, and a redirect
// is not.
//
// An upgrade to https on the same host is allowed, because it only ever narrows
// what the hop exposes. Everything else — another host, another port, and any
// downgrade to plaintext — is refused with a message that says what to change.
func SameOrigin(req *http.Request, via []*http.Request) error {
	if len(via) >= maxHops {
		return fmt.Errorf("too many redirects")
	}
	from, to := via[0].URL, req.URL
	if sameOrigin(from, to) {
		return nil
	}
	// Only the scheme and host, never the whole URL: with auth_style = "query"
	// the credential is in the URL, and this error is shown on screen and
	// pasted into bug reports.
	return fmt.Errorf("refusing a redirect from %s to %s: the credential and the request body are for the endpoint that was configured — point the base URL at the new one if that is where it has moved",
		origin(from), origin(to))
}

func sameOrigin(from, to *url.URL) bool {
	if !strings.EqualFold(from.Host, to.Host) {
		return false
	}
	if strings.EqualFold(from.Scheme, to.Scheme) {
		return true
	}
	// http -> https on the same host is the one change that cannot expose
	// anything the first request was not already exposing.
	return strings.EqualFold(from.Scheme, "http") && strings.EqualFold(to.Scheme, "https")
}

func origin(u *url.URL) string {
	if u == nil {
		return "an unknown address"
	}
	return u.Scheme + "://" + u.Host
}
