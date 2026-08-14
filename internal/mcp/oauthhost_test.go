package mcp

import "testing"

// The https requirement exists so a token never crosses the network in the
// clear, and every address it guards — issuer, authorization endpoint, token
// endpoint, registration endpoint — is read out of a document fetched from a
// server that has not been trusted yet. A host is exempt only when it is this
// machine, so the test that matters is the one that looks like it.
func TestOnlyRealLoopbackEscapesTheHTTPSRequirement(t *testing.T) {
	remote := []string{
		"http://127.evil.example/mcp",       // starts with "127."
		"http://127.0.0.1.evil.example/mcp", // and looks even more like it
		"http://127x.example/mcp",
		"http://localhost.evil.example/mcp",
		"http://notlocalhost/mcp",
		"http://1270.0.0.1/mcp",
		"http://8.8.8.8/mcp",
		"http://[2001:db8::1]/mcp",
	}
	for _, raw := range remote {
		if _, err := canonicalResource(raw); err == nil {
			t.Errorf("%s was accepted over plain http; the token would cross the network in the clear", raw)
		}
	}

	local := []string{
		"http://127.0.0.1:8080/mcp",
		"http://127.0.0.2:8080/mcp",
		"http://localhost:8080/mcp",
		"http://LOCALHOST:8080/mcp", // the host is not lower-cased before the check
		"http://localhost.:8080/mcp",
		"http://[::1]:8080/mcp",
	}
	for _, raw := range local {
		if _, err := canonicalResource(raw); err != nil {
			t.Errorf("%s is this machine and was refused: %v", raw, err)
		}
	}
}

// Only the port survives from a stored redirect URI. The listener is bound to
// loopback whatever the file said, so a URI naming anywhere else would have the
// code delivered there while this listener waited for something that never came.
func TestAStoredRedirectCannotMoveTheCallbackOffThisMachine(t *testing.T) {
	ln, redirect, err := listenForRedirect("http://evil.example:0/callback")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	if got := redirect[:len("http://127.0.0.1:")]; got != "http://127.0.0.1:" {
		t.Errorf("redirect_uri is %q; it must name this machine", redirect)
	}
}
