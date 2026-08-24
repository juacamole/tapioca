package provider

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"tapioca/internal/httpsafe"
)

// APIError is a non-2xx provider response, carrying enough structure for
// retry classification.
type APIError struct {
	Provider   string
	Status     int
	Message    string
	RetryAfter time.Duration
}

func (e *APIError) Error() string {
	return fmt.Sprintf("%s: HTTP %d: %s", e.Provider, e.Status, e.Message)
}

func newAPIError(providerName string, resp *http.Response, body []byte) *APIError {
	return &APIError{
		Provider:   providerName,
		Status:     resp.StatusCode,
		Message:    apiErrorText(body),
		RetryAfter: parseRetryAfter(resp.Header),
	}
}

func parseRetryAfter(h http.Header) time.Duration {
	if ms := h.Get("retry-after-ms"); ms != "" {
		if f, err := strconv.ParseFloat(ms, 64); err == nil && f > 0 {
			return time.Duration(f * float64(time.Millisecond))
		}
	}
	ra := h.Get("Retry-After")
	if ra == "" {
		return 0
	}
	if secs, err := strconv.ParseFloat(ra, 64); err == nil && secs > 0 {
		return time.Duration(secs * float64(time.Second))
	}
	if t, err := http.ParseTime(ra); err == nil {
		if d := time.Until(t); d > 0 {
			return d
		}
	}
	return 0
}

var overflowMarkers = []string{
	"context_length_exceeded",
	"prompt is too long",
	"context window",
	"maximum context length",
	"input length exceeds",
}

// IsContextOverflow reports whether the request was too large for the model;
// retrying can never help, compaction can.
func IsContextOverflow(err error) bool {
	var ae *APIError
	if !errors.As(err, &ae) {
		return false
	}
	if ae.Status == 413 {
		return true
	}
	msg := strings.ToLower(ae.Message)
	for _, m := range overflowMarkers {
		if strings.Contains(msg, m) {
			return true
		}
	}
	return false
}

// Retryable classifies transient failures: rate limits, server errors and
// network problems retry; auth, bad requests and oversized prompts do not.
func Retryable(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	if IsContextOverflow(err) {
		return false
	}
	var ae *APIError
	if errors.As(err, &ae) {
		switch {
		case ae.Status == 408, ae.Status == 429:
			return true
		case ae.Status >= 500:
			return true
		default:
			return false
		}
	}
	// *url.Error implements net.Error, so without unwrapping, permanent
	// failures (bad TLS cert, unsupported scheme) would burn the whole
	// retry schedule.
	var ue *url.Error
	if errors.As(err, &ue) {
		if ue.Timeout() {
			return true
		}
		var op *net.OpError
		if errors.As(ue.Err, &op) {
			return true
		}
		return errors.Is(ue.Err, io.EOF) || errors.Is(ue.Err, io.ErrUnexpectedEOF)
	}
	var ne net.Error
	if errors.As(err, &ne) {
		return true
	}
	s := err.Error()
	for _, marker := range []string{
		"connection refused", "connection reset", "broken pipe", "EOF",
		"no such host", "network is unreachable", "timeout",
		"reading stream",
	} {
		if strings.Contains(s, marker) {
			return true
		}
	}
	return false
}

const (
	RetryMaxAttempts    = 5
	retryBaseDelay      = 2 * time.Second
	retryMaxDelay       = 30 * time.Second
	retryMaxServerDelay = 5 * time.Minute
)

// RetryDelay computes the wait before attempt+1. ok is false when the server
// demands a longer wait than we are willing to sit through.
//
// attempt is floored at one because a shift by a negative amount is a runtime
// panic and not an error, and this function is called before its caller checks
// anything about the number it was given. The agent's retry loop counts from
// one and resets to -1 on a fallback so that its own ++ makes the next pass
// "the first attempt there" — which lands on zero, not one, so the fallback's
// first retryable failure took the whole process down. The caller has been
// corrected too; this is the half that makes the function total, since the
// caller is not the only one who can get the arithmetic wrong.
func RetryDelay(attempt int, err error) (time.Duration, bool) {
	if attempt < 1 {
		attempt = 1
	}
	// The shift overflows into zero or a negative for a large attempt, and a
	// zero delay makes the jitter below panic in Int63n. The loop's own budget
	// keeps attempt small today; the clamp is what keeps that from being the
	// only thing that does.
	d := retryBaseDelay << (attempt - 1)
	if d <= 0 || d > retryMaxDelay {
		d = retryMaxDelay
	}
	jitter := time.Duration(rand.Int63n(int64(d) / 4))
	d = d - d/8 + jitter

	var ae *APIError
	if errors.As(err, &ae) && ae.RetryAfter > 0 {
		if ae.RetryAfter > retryMaxServerDelay {
			return 0, false
		}
		if ae.RetryAfter > d {
			d = ae.RetryAfter + time.Duration(rand.Int63n(int64(time.Second)))
		}
	}
	return d, true
}

// httpClient is shared by all providers: no overall timeout (streams are
// long-lived), but connections and response headers must arrive promptly.
// Header timeout is generous because local servers may load a model first.
//
// CheckRedirect is the point of this client that is about security rather than
// patience. Without it the endpoint could answer any request with a 302 and
// net/http would carry x-api-key, api-key, a custom gateway's auth header and
// X-Amz-Security-Token to whatever host it named — it strips only Authorization
// and the cookie headers — and would replay the request body, the system prompt
// and every message, on a 307. See httpsafe.SameOrigin.
var httpClient = &http.Client{
	Transport: &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           (&net.Dialer{Timeout: 10 * time.Second}).DialContext,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 300 * time.Second,
	},
	CheckRedirect: httpsafe.SameOrigin,
}
