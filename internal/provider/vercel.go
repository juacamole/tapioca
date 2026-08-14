package provider

import (
	"maps"

	"tapioca/internal/config"
)

// The Vercel AI Gateway fronts models from every vendor behind one key and the
// OpenAI wire format, so there is no client to write — only an address, which
// is the whole point. Reaching it through the custom provider meant knowing the
// URL and typing it correctly; this is the same thing without that.

const (
	// The gateway's published address. The version segment is trimmed back off
	// by newOpenAILike, which adds its own.
	vercelBase = "https://ai-gateway.vercel.sh/v1"

	// Vercel's documented environment variable, so an exported key is found
	// without being configured twice.
	vercelKeyEnv = "AI_GATEWAY_API_KEY"
)

// vercelAttribution identifies Tapioca to the gateway. Both headers are
// optional and carry nothing about the user or the conversation — the gateway
// reads them to list the apps calling it. Config headers win, so a fork is not
// stuck announcing itself as this one.
func vercelAttribution(extra map[string]string) map[string]string {
	h := map[string]string{
		"http-referer": "https://github.com/juacamole/tapioca",
		"x-title":      "Tapioca",
	}
	maps.Copy(h, extra)
	return h
}

// NewVercel builds the gateway provider. The key is a plain bearer token, and
// base_url stays overridable for a proxy in front of the gateway even though
// the connect form never asks for it.
func NewVercel(name string, cfg config.ProviderConfig) *OpenAI {
	p := newOpenAILike(name, cfg, "", vercelBase, vercelKeyEnv)
	style := AuthBearer
	if p.apiKey == "" {
		// An empty bearer is worse than none: it reads as a malformed token
		// rather than a missing one, and the gateway's own 401 says more.
		style = AuthNone
	}
	p.auth = &authSpec{style: style, key: p.apiKey, extra: vercelAttribution(cfg.Headers)}
	return p
}
