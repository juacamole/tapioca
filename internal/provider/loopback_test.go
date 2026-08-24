package provider

import "testing"

// A prefix test for loopback is not one: "127.example.com" starts with "127."
// and resolves to whatever its owner points it at, so an http base_url anyone
// could suggest passed the "http is allowed for localhost" gate and sent every
// prompt, and the key, in the clear to a remote host.
func TestLoopbackIsNotAPrefixTest(t *testing.T) {
	if err := CheckBaseURL("http://api.example.com/v1"); err == nil {
		t.Fatal("control failed: plain http to a remote host was accepted")
	}
	for _, u := range []string{"http://127.example.com/v1", "http://127.0.0.1.evil.example/v1", "http://127.attacker.test:8080"} {
		if err := CheckBaseURL(u); err == nil {
			t.Errorf("CheckBaseURL(%q) accepted a remote host over http", u)
		}
	}
	for _, u := range []string{"http://localhost:1234/v1", "http://127.0.0.1:8080/v1", "http://[::1]:8080/v1"} {
		if err := CheckBaseURL(u); err != nil {
			t.Errorf("real loopback refused: %s: %v", u, err)
		}
	}
}
