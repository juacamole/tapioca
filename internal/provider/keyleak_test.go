package provider

import (
	"context"
	"strings"
	"testing"

	"tapioca/internal/config"
)

// With auth_style = "query" the credential is in the request URL, and
// net/http copies that URL into *url.Error verbatim. The error is shown in the
// status line and pasted into bug reports.
func TestQueryCredentialIsNotInTheError(t *testing.T) {
	const key = "sk-super-secret-key-12345"
	p, err := NewCustom("gw", config.ProviderConfig{
		BaseURL: "http://127.0.0.1:1/v1", APIKey: key,
		AuthStyle: AuthQuery, AuthQuery: "key",
	})
	if err != nil {
		t.Fatal(err)
	}
	_, lerr := p.ListModels(context.Background())
	if lerr == nil {
		t.Fatal("control failed: the request to a dead port succeeded")
	}
	if strings.Contains(lerr.Error(), key) {
		t.Errorf("the api key reached an error shown on screen: %v", lerr)
	}
}
