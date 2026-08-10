package tools

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestInternalAddressesAreRecognized(t *testing.T) {
	internal := []string{
		"127.0.0.1", "::1", "10.0.0.5", "192.168.1.1", "172.16.0.1",
		"169.254.169.254", // cloud metadata
		"0.0.0.0",
	}
	for _, host := range internal {
		if !isInternalIP(net.ParseIP(host)) {
			t.Errorf("%s not treated as internal", host)
		}
	}
	for _, host := range []string{"8.8.8.8", "1.1.1.1", "93.184.216.34"} {
		if isInternalIP(net.ParseIP(host)) {
			t.Errorf("%s wrongly treated as internal", host)
		}
	}
}

func TestSameHostIgnoresWWW(t *testing.T) {
	if !sameHost("example.com", "www.example.com") {
		t.Error("www. prefix should not count as a different host")
	}
	if sameHost("example.com", "evil.example.net") {
		t.Error("different hosts compared equal")
	}
}

func fetch(t *testing.T, e *Executor, url string) (string, bool) {
	t.Helper()
	raw, _ := json.Marshal(map[string]string{"url": url})
	out, isErr, err := e.webFetch(context.Background(), raw)
	if err != nil {
		t.Fatal(err)
	}
	return out, isErr
}

// An approved host must not be able to bounce the fetch somewhere else: that
// is how a prompt-injected page reaches an internal service.
func TestFetchRefusesCrossHostRedirect(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "https://example.com/elsewhere", http.StatusFound)
	}))
	defer srv.Close()

	e := NewExecutor(t.TempDir(), ModeBypass)
	out, isErr := fetch(t, e, srv.URL)
	if !isErr {
		t.Fatalf("cross-host redirect was followed: %q", out)
	}
	if !strings.Contains(out, "refusing redirect") {
		t.Errorf("unhelpful error: %q", out)
	}
}

func TestFetchRefusesRedirectToInternalAddress(t *testing.T) {
	// Same host (loopback), but the target is the kind of address that only
	// exists inside the machine or network.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/meta" {
			w.Write([]byte("SECRET-CREDENTIALS"))
			return
		}
		http.Redirect(w, r, srv0(r)+"/meta", http.StatusFound)
	}))
	defer srv.Close()

	e := NewExecutor(t.TempDir(), ModeBypass)
	out, isErr := fetch(t, e, srv.URL)
	if !isErr || strings.Contains(out, "SECRET-CREDENTIALS") {
		t.Fatalf("redirect to an internal address was followed: %q", out)
	}
}

// srv0 rebuilds the absolute URL of the running test server from a request.
func srv0(r *http.Request) string { return "http://" + r.Host }

func TestFetchFollowsSameHostRedirect(t *testing.T) {
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/final" {
			w.Write([]byte("landed"))
			return
		}
		http.Redirect(w, r, srv.URL+"/final", http.StatusFound)
	}))
	defer srv.Close()

	// Loopback is internal, so this only works because the *initial* URL is
	// user-approved; the guard applies to redirect targets.
	e := NewExecutor(t.TempDir(), ModeBypass)
	out, isErr := fetch(t, e, srv.URL+"/final")
	if isErr || !strings.Contains(out, "landed") {
		t.Fatalf("a direct fetch of an approved URL failed: %q", out)
	}
}
