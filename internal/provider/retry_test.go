package provider

import (
	"crypto/x509"
	"errors"
	"net"
	"net/url"
	"testing"
	"time"
)

func TestRetryableClassification(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"rate limit", &APIError{Status: 429}, true},
		{"server error", &APIError{Status: 500}, true},
		{"truncated stream", &APIError{Status: 502, Message: "stream ended before completion"}, true},
		{"bad request", &APIError{Status: 400}, false},
		{"auth", &APIError{Status: 401}, false},
		{"context overflow by status", &APIError{Status: 413}, false},
		{"context overflow by message", &APIError{Status: 400, Message: "prompt is too long: 210000 tokens"}, false},
		{"dial failure", &url.Error{Op: "Post", Err: &net.OpError{Op: "dial", Err: errors.New("connection refused")}}, true},
		{"bad tls cert", &url.Error{Op: "Post", Err: x509.UnknownAuthorityError{}}, false},
		{"unsupported scheme", &url.Error{Op: "Post", Err: errors.New("unsupported protocol scheme \"htp\"")}, false},
	}
	for _, c := range cases {
		if got := Retryable(c.err); got != c.want {
			t.Errorf("%s: Retryable=%v, want %v", c.name, got, c.want)
		}
	}
}

func TestRetryDelayHonorsServer(t *testing.T) {
	d, ok := RetryDelay(1, &APIError{Status: 429, RetryAfter: 12 * time.Second})
	if !ok || d < 12*time.Second {
		t.Fatalf("server delay not honored: %v ok=%v", d, ok)
	}
	if _, ok := RetryDelay(1, &APIError{Status: 429, RetryAfter: 10 * time.Minute}); ok {
		t.Fatal("excessive server delay should abort retrying")
	}
}
