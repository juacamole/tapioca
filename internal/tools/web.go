package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"regexp"
	"strings"
	"syscall"
	"time"

	"tapioca/internal/textenc"
)

// Web tools: keyless search via DuckDuckGo's HTML endpoint and a readable
// page fetcher. Both are non-mutating and skip the permission gate.

// The host of a fetch is approved by the user, but redirects are not: without
// this a page on an approved host can bounce the fetch to the cloud metadata
// endpoint or anything else on the local network. Redirects must stay on the
// approved host and may never land on an internal address.
var webClient = &http.Client{
	Timeout:   25 * time.Second,
	Transport: screenedTransport(),
	CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) >= 5 {
			return fmt.Errorf("too many redirects")
		}
		origin := via[0].URL.Hostname()
		if !sameHost(origin, req.URL.Hostname()) {
			return fmt.Errorf("refusing redirect to %s — fetch that URL directly so it can be approved",
				req.URL.Hostname())
		}
		if internalHost(req.URL.Hostname()) {
			return fmt.Errorf("refusing redirect to the internal address %s", req.URL.Hostname())
		}
		return nil
	},
}

func sameHost(a, b string) bool {
	return strings.EqualFold(strings.TrimPrefix(a, "www."), strings.TrimPrefix(b, "www."))
}

// screenedTransport is the default transport with the address it is about to
// connect to screened first.
//
// A check on the host name cannot be the boundary. webFetch resolves the name
// to decide whether to allow it and then hands the name to the transport,
// which resolves it again; nothing makes the two answers the same. A record
// with a zero TTL, or a name server that simply answers differently the second
// time, is screened as one address and connected to as another — the ordinary
// DNS rebinding move, and 169.254.169.254 is what it is worth here. Control
// runs after resolution and before connect, with the address that will
// actually be used, which is the only place the answer cannot change again.
//
// A clone of the default rather than a fresh Transport, so proxies, HTTP/2 and
// the connection pooling a plain http.Client would have had all stay in place.
func screenedTransport() http.RoundTripper {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.DialContext = (&net.Dialer{
		Timeout:   15 * time.Second,
		KeepAlive: 30 * time.Second,
		Control:   refuseLinkLocal,
	}).DialContext
	return tr
}

// refuseLinkLocal is the policy webFetch states, applied to the address rather
// than to the name. Only link-local, matching linkLocalHost: loopback and the
// private ranges are refused nowhere, because a dev server on localhost and a
// machine on the LAN are the ordinary reasons to fetch a private address.
//
// An address that does not parse is refused. Control is only ever handed a
// literal, so a value that is not one means something upstream is not what
// this assumes, and that is not a state to connect in.
func refuseLinkLocal(_, address string, _ syscall.RawConn) error {
	host := address
	if h, _, err := net.SplitHostPort(address); err == nil {
		host = h
	}
	ip, err := netip.ParseAddr(strings.Trim(host, "[]"))
	if err != nil {
		return fmt.Errorf("refusing to connect to %q: not an address", address)
	}
	if ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() {
		return fmt.Errorf("refusing to connect to the link-local address %s (cloud metadata lives there)", ip)
	}
	return nil
}

// internalHost reports addresses that only mean something inside this machine
// or network: loopback, link-local (169.254.169.254 is the cloud metadata
// service), and the private ranges.
func internalHost(host string) bool {
	ip := net.ParseIP(host)
	if ip == nil {
		// A name, not an address: resolve it, since a public name can point
		// anywhere. Failure to resolve is left to the request itself.
		addrs, err := net.LookupIP(host)
		if err != nil {
			return false
		}
		for _, a := range addrs {
			if isInternalIP(a) {
				return true
			}
		}
		return false
	}
	return isInternalIP(ip)
}

// linkLocalHost is internalHost narrowed to the addresses an agent has no
// business fetching at all.
func linkLocalHost(host string) bool {
	ip := net.ParseIP(host)
	if ip == nil {
		addrs, err := net.LookupIP(host)
		if err != nil {
			return false
		}
		for _, a := range addrs {
			if a.IsLinkLocalUnicast() || a.IsLinkLocalMulticast() {
				return true
			}
		}
		return false
	}
	return ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast()
}

func isInternalIP(ip net.IP) bool {
	return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() || ip.IsUnspecified()
}

const webUA = "Mozilla/5.0 (X11; Linux x86_64) tapioca/0.1"

var (
	ddgResultRe  = regexp.MustCompile(`(?s)<a[^>]*class="result__a"[^>]*href="([^"]+)"[^>]*>(.*?)</a>`)
	ddgSnippetRe = regexp.MustCompile(`(?s)class="result__snippet"[^>]*>(.*?)</a>`)
	tagRe        = regexp.MustCompile(`<[^>]*>`)
	scriptRe     = regexp.MustCompile(`(?is)<(script|style|noscript|head)[^>]*>.*?</\s*(script|style|noscript|head)\s*>`)
	brRe         = regexp.MustCompile(`(?i)<(br|/p|/div|/li|/h[1-6]|/tr|/section|/article)[^>]*>`)
	blankRe      = regexp.MustCompile(`\n{3,}`)
	spaceRe      = regexp.MustCompile(`[ \t]{2,}`)
)

func (e *Executor) webSearch(ctx context.Context, raw json.RawMessage) (string, bool, error) {
	var a struct {
		Query      string `json:"query"`
		MaxResults int    `json:"max_results"`
	}
	if err := json.Unmarshal(raw, &a); err != nil || strings.TrimSpace(a.Query) == "" {
		return "invalid arguments: need {\"query\": \"...\"}", true, nil
	}
	if a.MaxResults <= 0 || a.MaxResults > 10 {
		a.MaxResults = 5
	}

	req, err := http.NewRequestWithContext(ctx, "GET",
		"https://html.duckduckgo.com/html/?q="+url.QueryEscape(a.Query), nil)
	if err != nil {
		return err.Error(), true, nil
	}
	req.Header.Set("User-Agent", webUA)
	resp, err := webClient.Do(req)
	if err != nil {
		return "search failed: " + err.Error(), true, nil
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Sprintf("search failed: HTTP %d", resp.StatusCode), true, nil
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))

	page := string(body)
	locs := ddgResultRe.FindAllStringSubmatchIndex(page, a.MaxResults)
	if len(locs) == 0 {
		return "no results for: " + a.Query, false, nil
	}
	var b strings.Builder
	for i, loc := range locs {
		title := cleanHTML(page[loc[4]:loc[5]])
		target := decodeDDGHref(page[loc[2]:loc[3]])
		fmt.Fprintf(&b, "%d. %s\n   %s\n", i+1, title, target)
		// Snippets pair by position: search only between this result and
		// the next, so a snippetless hit can't shift later attributions.
		end := len(page)
		if i+1 < len(locs) {
			end = locs[i+1][0]
		}
		if sm := ddgSnippetRe.FindStringSubmatch(page[loc[1]:end]); sm != nil {
			if s := cleanHTML(sm[1]); s != "" {
				fmt.Fprintf(&b, "   %s\n", s)
			}
		}
	}
	return b.String(), false, nil
}

// decodeDDGHref unwraps DuckDuckGo's redirect links to the real URL.
func decodeDDGHref(href string) string {
	href = html.UnescapeString(href)
	if strings.HasPrefix(href, "//") {
		href = "https:" + href
	}
	if u, err := url.Parse(href); err == nil {
		if target := u.Query().Get("uddg"); target != "" {
			if dec, err := url.QueryUnescape(target); err == nil {
				return dec
			}
		}
	}
	return href
}

func (e *Executor) webFetch(ctx context.Context, raw json.RawMessage) (string, bool, error) {
	var a struct {
		URL string `json:"url"`
	}
	if err := json.Unmarshal(raw, &a); err != nil || strings.TrimSpace(a.URL) == "" {
		return "invalid arguments: need {\"url\": \"...\"}", true, nil
	}
	target := strings.TrimSpace(a.URL)
	if !strings.HasPrefix(target, "http://") && !strings.HasPrefix(target, "https://") {
		target = "https://" + target
	}
	req, err := http.NewRequestWithContext(ctx, "GET", target, nil)
	if err != nil {
		return err.Error(), true, nil
	}
	// Redirects were screened for internal addresses and the URL the model
	// chose was not, so the cloud metadata endpoint was reachable by asking
	// for it directly. Only link-local is refused here, not every internal
	// address: fetching your own dev server on localhost is a real thing to
	// want, and it goes through the per-host prompt like anything else.
	// 169.254.169.254 is not, and no prompt describes it as what it is.
	if linkLocalHost(req.URL.Hostname()) {
		return "refusing to fetch the link-local address " + req.URL.Hostname() +
			" (cloud metadata lives there)", true, nil
	}
	req.Header.Set("User-Agent", webUA)
	resp, err := webClient.Do(req)
	if err != nil {
		return "fetch failed: " + err.Error(), true, nil
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	text, isText := textenc.Decode(body)
	if !isText {
		return fmt.Sprintf("binary response (%d bytes, %s) — not showing raw contents",
			len(body), resp.Header.Get("Content-Type")), true, nil
	}
	if strings.Contains(resp.Header.Get("Content-Type"), "html") ||
		strings.Contains(strings.ToLower(text[:min(len(text), 512)]), "<html") {
		text = cleanHTML(text)
	}
	text = strings.TrimSpace(text)
	if text == "" {
		text = fmt.Sprintf("(empty body, HTTP %d)", resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK {
		text = fmt.Sprintf("HTTP %d\n%s", resp.StatusCode, text)
	}
	const capLen = 20_000
	if len(text) > capLen {
		text = text[:capLen] + "\n[truncated]"
	}
	return text, resp.StatusCode >= 400, nil
}

// cleanHTML reduces markup to readable text.
func cleanHTML(s string) string {
	s = scriptRe.ReplaceAllString(s, " ")
	s = brRe.ReplaceAllString(s, "\n")
	s = tagRe.ReplaceAllString(s, "")
	s = html.UnescapeString(s)
	var lines []string
	for _, l := range strings.Split(s, "\n") {
		lines = append(lines, strings.TrimSpace(spaceRe.ReplaceAllString(l, " ")))
	}
	s = strings.Join(lines, "\n")
	return strings.TrimSpace(blankRe.ReplaceAllString(s, "\n\n"))
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
