package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// Web tools: keyless search via DuckDuckGo's HTML endpoint and a readable
// page fetcher. Both are non-mutating and skip the permission gate.

var webClient = &http.Client{Timeout: 25 * time.Second}

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

	links := ddgResultRe.FindAllStringSubmatch(string(body), a.MaxResults)
	snippets := ddgSnippetRe.FindAllStringSubmatch(string(body), a.MaxResults)
	if len(links) == 0 {
		return "no results for: " + a.Query, false, nil
	}
	var b strings.Builder
	for i, l := range links {
		title := cleanHTML(l[2])
		target := decodeDDGHref(l[1])
		fmt.Fprintf(&b, "%d. %s\n   %s\n", i+1, title, target)
		if i < len(snippets) {
			if s := cleanHTML(snippets[i][1]); s != "" {
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
	req.Header.Set("User-Agent", webUA)
	resp, err := webClient.Do(req)
	if err != nil {
		return "fetch failed: " + err.Error(), true, nil
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	text := string(body)
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
