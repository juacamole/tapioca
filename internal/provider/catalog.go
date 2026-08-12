package provider

import "strings"

// The config only lists providers someone has already set up, which is no help
// to the person who has set up none: there was no way to learn what Tapioca can
// even connect to without reading the source. The catalog is that list, held
// separately from the config so the UI can offer a provider before it exists.

// Field is one value a provider needs before it can be used.
type Field struct {
	Key      string // the config key it is written to
	Label    string
	Help     string // where the value comes from, in the user's words
	Secret   bool   // masked on entry and never echoed anywhere
	Optional bool
	Default  string
}

// Kind describes a provider type: what to call it, and what it needs.
type Kind struct {
	Type   string // the config `type` value
	Label  string
	Desc   string
	URL    string // where credentials are issued, empty when there are none
	Fields []Field
}

// NeedsSetup reports whether this kind requires anything at all — Ollama is
// the one that works with nothing.
func (k Kind) NeedsSetup() bool { return len(k.Fields) > 0 }

// Catalog is every provider type that can be connected, in the order the UI
// offers them: local first, then the ones most people reach for.
var Catalog = []Kind{
	{
		Type:  "ollama",
		Label: "ollama",
		Desc:  "models running on this machine — no account, no key",
		Fields: []Field{
			{Key: "base_url", Label: "address", Help: "where Ollama is listening", Optional: true, Default: "http://localhost:11434"},
		},
	},
	{
		Type:  "anthropic",
		Label: "anthropic",
		Desc:  "Claude models",
		URL:   "https://console.anthropic.com/settings/keys",
		Fields: []Field{
			{Key: "api_key", Label: "API key", Help: "console.anthropic.com → Settings → API keys", Secret: true},
		},
	},
	{
		Type:  "openai",
		Label: "openai",
		Desc:  "GPT models, and anything speaking the same API",
		URL:   "https://platform.openai.com/api-keys",
		Fields: []Field{
			{Key: "api_key", Label: "API key", Help: "platform.openai.com → API keys", Secret: true},
			{Key: "base_url", Label: "address", Help: "only for a compatible server that is not OpenAI", Optional: true},
		},
	},
	{
		Type:  "gemini",
		Label: "gemini",
		Desc:  "Google's Gemini models",
		URL:   "https://aistudio.google.com/apikey",
		Fields: []Field{
			{Key: "api_key", Label: "API key", Help: "aistudio.google.com → Get API key", Secret: true},
		},
	},
	{
		Type:  "bedrock",
		Label: "bedrock",
		Desc:  "Claude and others through AWS",
		Fields: []Field{
			{Key: "region", Label: "region", Help: "the AWS region the models are enabled in", Default: "us-east-1"},
			{Key: "profile", Label: "profile", Help: "a profile from ~/.aws/config; blank uses the default chain", Optional: true},
		},
	},
	{
		Type:  "vertex",
		Label: "vertex",
		Desc:  "Claude through Google Cloud",
		Fields: []Field{
			{Key: "project", Label: "project", Help: "the GCP project id"},
			{Key: "region", Label: "region", Help: "or 'global'", Default: "global"},
			{Key: "credentials_file", Label: "service account", Help: "path to a JSON key; blank uses gcloud application-default credentials", Optional: true},
		},
	},
	{
		Type:  "azure",
		Label: "azure",
		Desc:  "OpenAI models through Azure",
		Fields: []Field{
			{Key: "base_url", Label: "endpoint", Help: "https://<resource>.openai.azure.com"},
			{Key: "api_key", Label: "API key", Help: "the resource's key in the Azure portal", Secret: true},
			{Key: "api_version", Label: "API version", Help: "blank uses a known-good one", Optional: true},
		},
	},
}

// Needs describes what a provider is missing, for the person who has not set
// it up: the required fields by name, and where the credential comes from.
func (k Kind) Needs() string {
	var req []string
	for _, f := range k.Fields {
		if !f.Optional {
			req = append(req, f.Label)
		}
	}
	if len(req) == 0 {
		return k.Label + " needs no credentials"
	}
	out := k.Label + " needs " + joinWords(req)
	if k.URL != "" {
		out += " — " + k.URL
	}
	return out
}

func joinWords(w []string) string {
	switch len(w) {
	case 0:
		return ""
	case 1:
		return w[0]
	case 2:
		return w[0] + " and " + w[1]
	}
	return strings.Join(w[:len(w)-1], ", ") + " and " + w[len(w)-1]
}

// KindFor returns the catalog entry for a config type, matching the aliases
// New accepts so a provider written as "google" or "openai-compatible" is not
// reported as unknown.
func KindFor(typ string) (Kind, bool) {
	switch typ {
	case "":
		typ = "ollama"
	case "google":
		typ = "gemini"
	case "openai-compatible":
		typ = "openai"
	}
	for _, k := range Catalog {
		if k.Type == typ {
			return k, true
		}
	}
	return Kind{}, false
}
