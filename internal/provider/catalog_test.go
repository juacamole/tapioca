package provider

import (
	"os"
	"regexp"
	"strings"
	"testing"

	"tapioca/internal/config"
)

// The catalog is a second list of provider types, and a second list drifts:
// add a case to New and forget the catalog entry, and the provider exists but
// can never be offered — invisible, with nothing failing to point at it. So
// the test reads New's own switch rather than a list maintained by hand here.
func TestCatalogCoversEveryTypeNewAccepts(t *testing.T) {
	src, err := os.ReadFile("provider.go")
	if err != nil {
		t.Fatal(err)
	}
	fn := string(src)
	start := strings.Index(fn, "func New(name string, cfg config.ProviderConfig)")
	if start < 0 {
		t.Fatal("could not find New in provider.go — this test needs updating")
	}
	body := fn[start:]
	if end := strings.Index(body, "\n}"); end > 0 {
		body = body[:end]
	}

	caseLine := regexp.MustCompile(`(?m)^\tcase (.+):$`)
	quoted := regexp.MustCompile(`"([^"]*)"`)
	found := 0
	for _, line := range caseLine.FindAllStringSubmatch(body, -1) {
		for _, q := range quoted.FindAllStringSubmatch(line[1], -1) {
			found++
			if _, ok := KindFor(q[1]); !ok {
				t.Errorf("provider.New accepts type %q, but the catalog has no entry for it — "+
					"it can never be offered in the connect screen", q[1])
			}
		}
	}
	if found < 5 {
		t.Fatalf("parsed only %d case values from New; the test is not reading the switch", found)
	}
}

// Every catalog entry must be constructible, or the screen offers a provider
// that cannot exist.
func TestEveryCatalogTypeIsKnownToNew(t *testing.T) {
	for _, k := range Catalog {
		_, err := New(k.Label, minimalConfigFor(k))
		if err != nil && strings.Contains(err.Error(), "unknown type") {
			t.Errorf("catalog offers %q, which New rejects as unknown", k.Type)
		}
	}
}

// minimalConfigFor fills required fields with placeholders so construction
// gets far enough to reject an unknown type, without reaching the network.
func minimalConfigFor(k Kind) config.ProviderConfig {
	c := config.ProviderConfig{Type: k.Type}
	for _, f := range k.Fields {
		if f.Optional {
			continue
		}
		switch f.Key {
		case "api_key":
			c.APIKey = "placeholder"
		case "base_url":
			c.BaseURL = "http://localhost:0"
		case "region":
			c.Region = "us-east-1"
		case "project":
			c.Project = "placeholder"
		}
	}
	return c
}

func TestKindForResolvesTheAliasesNewAccepts(t *testing.T) {
	cases := map[string]string{
		"":                  "ollama", // an omitted type means ollama
		"google":            "gemini",
		"openai-compatible": "openai",
		"anthropic":         "anthropic",
	}
	for in, want := range cases {
		k, ok := KindFor(in)
		if !ok {
			t.Errorf("KindFor(%q) found nothing", in)
			continue
		}
		if k.Type != want {
			t.Errorf("KindFor(%q) = %q, want %q", in, k.Type, want)
		}
	}
	if _, ok := KindFor("nonsense"); ok {
		t.Error("KindFor accepted a type that does not exist")
	}
}

// A field the entry flow does not know is secret is a field it will echo.
func TestCredentialFieldsAreMarkedSecret(t *testing.T) {
	for _, k := range Catalog {
		for _, f := range k.Fields {
			looksSecret := strings.Contains(strings.ToLower(f.Key), "key") ||
				strings.Contains(strings.ToLower(f.Key), "token") ||
				strings.Contains(strings.ToLower(f.Key), "secret")
			if looksSecret && !f.Secret {
				t.Errorf("%s.%s looks like a credential but is not marked Secret", k.Type, f.Key)
			}
		}
	}
}

// Needs is what an unconfigured provider tells the user, so it has to name the
// required fields and, where one exists, where to get the credential.
func TestNeedsNamesTheRequiredFields(t *testing.T) {
	for _, k := range Catalog {
		got := k.Needs()
		for _, f := range k.Fields {
			if f.Optional {
				continue
			}
			if !strings.Contains(got, f.Label) {
				t.Errorf("%s.Needs() = %q, missing required field %q", k.Type, got, f.Label)
			}
		}
		if k.URL != "" && !strings.Contains(got, k.URL) {
			t.Errorf("%s.Needs() omits where to get the credential", k.Type)
		}
	}
}
