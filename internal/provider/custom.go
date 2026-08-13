package provider

import (
	"fmt"
	"os"
	"strings"

	"tapioca/internal/config"
)

// A custom provider is any server speaking the OpenAI wire format, which is
// most of them — OpenRouter, Groq, Together, DeepSeek, Fireworks, vLLM, LM
// Studio, gateways. What varies is not the format but where the credential
// goes, so that part is configuration rather than a new client each time.

// NewCustom builds a provider from an address and an auth style.
func NewCustom(name string, cfg config.ProviderConfig) (*OpenAI, error) {
	base := strings.TrimSuffix(strings.TrimSpace(cfg.BaseURL), "/")
	if err := CheckBaseURL(base); err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}

	key := cfg.APIKey
	if key == "" && cfg.APIKeyEnv != "" {
		key = os.Getenv(cfg.APIKeyEnv)
	}

	style := strings.ToLower(strings.TrimSpace(cfg.AuthStyle))
	if style == "" {
		style = AuthBearer
	}
	if !knownAuthStyle(style) {
		return nil, fmt.Errorf("%s: auth_style %q is not one of bearer, header, query or none", name, style)
	}
	// A blank key with a style that needs one is a provider that will fail on
	// its first request with a message from the far end. Saying so here names
	// the actual problem.
	if style != AuthNone && key == "" {
		return nil, fmt.Errorf("%s: no credential (set api_key, api_key_env, or auth_style = \"none\")", name)
	}

	spec := &authSpec{
		style:  style,
		header: strings.TrimSpace(cfg.AuthHeader),
		query:  strings.TrimSpace(cfg.AuthQuery),
		key:    key,
		extra:  cfg.Headers,
	}
	// Fail here rather than on every request: a header that cannot be sent is
	// a configuration error, and the place to report it is where it was
	// configured.
	if err := spec.validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}

	p := NewOpenAI(name, cfg)
	p.baseURL = trimVersionSuffix(base)
	p.auth = spec
	return p, nil
}

func knownAuthStyle(s string) bool {
	for _, a := range AuthStyles {
		if a.Name == s {
			return true
		}
	}
	return false
}

// validate checks what apply would reject, without needing a request.
func (a authSpec) validate() error {
	switch a.style {
	case AuthHeader:
		if a.header == "" {
			return fmt.Errorf("auth_style is %q but auth_header is not set", AuthHeader)
		}
		if err := validHeader(a.header, a.key); err != nil {
			return err
		}
	case AuthQuery:
		if a.query == "" {
			return fmt.Errorf("auth_style is %q but auth_query is not set", AuthQuery)
		}
	}
	for name, value := range a.extra {
		if err := validHeader(name, value); err != nil {
			return err
		}
		if strings.EqualFold(name, "Authorization") {
			return fmt.Errorf("header %q would replace the credential; set auth_style instead", name)
		}
	}
	return nil
}
