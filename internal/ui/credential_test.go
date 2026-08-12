package ui

import (
	"reflect"
	"strings"
	"testing"

	"tapioca/internal/config"
	"tapioca/internal/provider"
)

// applyField is a switch over catalog field keys, which is the shape that has
// bitten this project before: add a field to the catalog, forget the case, and
// the value is silently dropped — the form accepts it, the test passes because
// the provider was already working, and the setting never applies. So every
// field the catalog declares must actually land somewhere in the config.
func TestEveryCatalogFieldReachesTheConfig(t *testing.T) {
	const sentinel = "sentinel-value-9137"
	for _, k := range provider.Catalog {
		for _, f := range k.Fields {
			var pc config.ProviderConfig
			applyField(&pc, f.Key, sentinel)
			if !containsString(pc, sentinel) {
				t.Errorf("%s.%s: applyField has no case for it, so the value is dropped",
					k.Type, f.Key)
			}
		}
	}
}

func containsString(v any, want string) bool {
	rv := reflect.ValueOf(v)
	for i := 0; i < rv.NumField(); i++ {
		f := rv.Field(i)
		if f.Kind() == reflect.String && f.String() == want {
			return true
		}
	}
	return false
}

// A key already exported in the environment should be referenced, not copied:
// the config file is read by anything the user points at their dotfiles, and a
// second copy is a second place to leak from.
func TestSavePrefersAnEnvironmentReference(t *testing.T) {
	const key = "sk-test-envref-2f8c"
	t.Setenv("TEST_PROVIDER_API_KEY", key)

	m := &App{cfg: config.Default()}
	m.cfg.Providers = map[string]config.ProviderConfig{}
	k, _ := provider.KindFor("openai")
	field := provider.Field{Key: "api_key", Secret: true}

	m.saveCredential(k, field, key)

	pc := m.cfg.Providers["openai"]
	if pc.APIKey != "" {
		t.Errorf("the literal key was written to the config despite being in the environment")
	}
	if pc.APIKeyEnv != "TEST_PROVIDER_API_KEY" {
		t.Errorf("APIKeyEnv = %q, want the variable already holding the key", pc.APIKeyEnv)
	}
}

// When nothing in the environment holds it, a reference would name an unset
// variable — a provider that silently does not work. The literal is correct.
func TestSaveFallsBackToTheLiteral(t *testing.T) {
	m := &App{cfg: config.Default()}
	m.cfg.Providers = map[string]config.ProviderConfig{}
	k, _ := provider.KindFor("openai")
	field := provider.Field{Key: "api_key", Secret: true}

	m.saveCredential(k, field, "sk-not-in-the-environment-4a1b")

	pc := m.cfg.Providers["openai"]
	if pc.APIKey != "sk-not-in-the-environment-4a1b" {
		t.Errorf("APIKey = %q, want the literal", pc.APIKey)
	}
	if pc.APIKeyEnv != "" {
		t.Errorf("APIKeyEnv = %q, want empty — it would name an unset variable", pc.APIKeyEnv)
	}
}

// Only variables that look like credentials are matched. Otherwise a key that
// happens to equal $PWD or $TERM would be "referenced" from a variable that
// has nothing to do with it and changes without warning.
func TestEnvHoldingIgnoresUnrelatedVariables(t *testing.T) {
	t.Setenv("SOME_UNRELATED_PATH", "shared-value-11a")
	if got := envHolding("shared-value-11a"); got != "" {
		t.Errorf("envHolding matched %q, which is not a credential variable", got)
	}
	t.Setenv("OTHER_API_KEY", "shared-value-11a")
	if got := envHolding("shared-value-11a"); got != "OTHER_API_KEY" {
		t.Errorf("envHolding = %q, want OTHER_API_KEY", got)
	}
}

// The secret must not be reachable from the message that carries the test
// result: a value in a message is a value in a panic dump.
func TestTestedMessageCarriesNoSecret(t *testing.T) {
	rt := reflect.TypeOf(credTestedMsg{})
	for i := 0; i < rt.NumField(); i++ {
		name := strings.ToLower(rt.Field(i).Name)
		if strings.Contains(name, "key") || strings.Contains(name, "value") ||
			strings.Contains(name, "secret") || strings.Contains(name, "token") {
			t.Errorf("credTestedMsg.%s looks like it carries the credential", rt.Field(i).Name)
		}
	}
}

// A secret field must be entered with echo off. This is the one property that
// cannot be caught by reading the screen in review, because it only shows when
// someone is watching.
func TestSecretFieldsAreEnteredWithEchoOff(t *testing.T) {
	m := &App{w: 100}
	for _, k := range provider.Catalog {
		var secret bool
		for _, f := range k.Fields {
			if !f.Optional && f.Secret {
				secret = true
			}
		}
		if !secret {
			continue
		}
		m.openCredentialEntry(k)
		if m.cred == nil {
			t.Fatalf("%s: no form opened", k.Type)
		}
		if m.cred.input.EchoMode == 0 {
			t.Errorf("%s: the %s field echoes what is typed", k.Type, m.cred.field.Label)
		}
	}
}
