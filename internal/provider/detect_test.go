package provider

import "testing"

// A local server needs nothing described before it can be looked for; a hosted
// one cannot be tried without a credential, and probing its default address
// would send an unprompted request to a third party.
func TestDetectableOnlyCoversLocalCredentiallessKinds(t *testing.T) {
	want := map[string]bool{
		"ollama": true, "llamacpp": true,
		"openai": false, "anthropic": false, "gemini": false,
		"azure": false, "bedrock": false, "vertex": false,
		"vercel": false, "custom": false,
	}
	for _, k := range Catalog {
		expect, known := want[k.Type]
		if !known {
			t.Errorf("catalog gained %q with no expectation recorded here", k.Type)
			continue
		}
		if got := k.Detectable(); got != expect {
			t.Errorf("%s.Detectable() = %v, want %v", k.Type, got, expect)
		}
	}
}

// custom declares base_url without Optional, so it needs the user before it
// can be tried at all — the distinction Detectable rests on.
func TestNeedsCredentialsIsAboutRequiredFields(t *testing.T) {
	for typ, want := range map[string]bool{
		"llamacpp": false, "ollama": false, "custom": true, "openai": true,
	} {
		k, ok := KindFor(typ)
		if !ok {
			t.Fatalf("no kind %q", typ)
		}
		if got := k.NeedsCredentials(); got != want {
			t.Errorf("%s.NeedsCredentials() = %v, want %v", typ, got, want)
		}
	}
}

func TestDefaultAddress(t *testing.T) {
	k, _ := KindFor("llamacpp")
	if got := k.DefaultAddress(); got != "http://localhost:8080" {
		t.Errorf("llamacpp default address = %q", got)
	}
	k, _ = KindFor("anthropic")
	if got := k.DefaultAddress(); got != "" {
		t.Errorf("anthropic has no address of its own, got %q", got)
	}
}
