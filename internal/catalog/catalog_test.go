package catalog

import "testing"

// Gateways namespace their model ids by vendor — the Vercel AI Gateway serves
// "anthropic/claude-opus-5", and OpenRouter, Together and Fireworks all use the
// same convention. models.dev keys the same model bare, so a lookup on the
// gateway's id missed and the model came back unknown: no price, and a context
// window that fell through to a default small enough to trigger compaction
// almost immediately.
const gatewayFixture = `{
  "anthropic": {
    "models": {
      "claude-opus-5": {
        "limit": {"context": 200000, "output": 64000},
        "cost": {"input": 5, "output": 25, "cache_read": 0.5, "cache_write": 6.25}
      }
    }
  }
}`

func seed(t *testing.T) {
	t.Helper()
	m := parse([]byte(gatewayFixture))
	if len(m) == 0 {
		t.Fatal("fixture did not parse — the test is not exercising anything")
	}
	mu.Lock()
	prev := hosted
	hosted = m
	mu.Unlock()
	t.Cleanup(func() {
		mu.Lock()
		hosted = prev
		mu.Unlock()
	})
}

func TestLookupResolvesVendorPrefixedIDs(t *testing.T) {
	seed(t)

	bare, ok := Lookup("claude-opus-5")
	if !ok {
		t.Fatal("the bare id does not resolve — the fixture is wrong, not the code")
	}

	for _, id := range []string{
		"anthropic/claude-opus-5", // Vercel AI Gateway, OpenRouter
		"Anthropic/Claude-Opus-5", // case is already normalized away
	} {
		got, ok := Lookup(id)
		if !ok {
			t.Errorf("Lookup(%q) found nothing; a gateway model has no price and no context window", id)
			continue
		}
		if got != bare {
			t.Errorf("Lookup(%q) = %+v, want the same entry as the bare id %+v", id, got, bare)
		}
	}
}

// The prefix fallback must not invent matches. Stripping to the last segment is
// only reasonable when the whole id is unknown; a model that genuinely is not in
// the catalog has to stay unknown, or every unrecognized id acquires whichever
// price happens to share its final segment.
func TestLookupDoesNotInventMatches(t *testing.T) {
	seed(t)

	for _, id := range []string{
		"anthropic/some-model-that-does-not-exist",
		"claude-opus-5/not-a-real-model",
		"",
	} {
		if _, ok := Lookup(id); ok {
			t.Errorf("Lookup(%q) matched something it should not have", id)
		}
	}
}

// An exact hit must win, so a catalog that ever does key a full vendor/model id
// is not silently overridden by its own last segment.
func TestLookupPrefersTheExactID(t *testing.T) {
	seed(t)
	mu.Lock()
	hosted["vendor/claude-opus-5"] = Model{Context: 111, In: 9}
	mu.Unlock()

	got, ok := Lookup("vendor/claude-opus-5")
	if !ok {
		t.Fatal("the exact id did not resolve")
	}
	if got.Context != 111 {
		t.Errorf("Lookup returned the stripped entry (context %d), want the exact one (111)", got.Context)
	}
}
