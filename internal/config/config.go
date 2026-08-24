// Package config loads and writes Tapioca's TOML configuration.
package config

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/BurntSushi/toml"
)

// ProviderConfig describes one LLM backend.
type ProviderConfig struct {
	Type            string            `toml:"type"`             // "ollama" | "llamacpp" | "anthropic" | "openai" | …
	BaseURL         string            `toml:"base_url"`         // optional override
	APIKey          string            `toml:"api_key"`          // literal key (discouraged; prefer env)
	APIKeyEnv       string            `toml:"api_key_env"`      // env var holding the key
	Auth            string            `toml:"auth"`             // "oauth" authenticates from the provider CLI profile
	AuthStyle       string            `toml:"auth_style"`       // custom: bearer | header | query | none
	AuthHeader      string            `toml:"auth_header"`      // custom: header name when auth_style = header
	AuthQuery       string            `toml:"auth_query"`       // custom: query parameter when auth_style = query
	Headers         map[string]string `toml:"headers"`          // custom: sent on every request
	ContextWindow   int               `toml:"context_window"`   // tokens; 0 = per-type default
	APIVersion      string            `toml:"api_version"`      // azure only; defaults to a known-good one
	Region          string            `toml:"region"`           // bedrock, vertex
	Profile         string            `toml:"profile"`          // bedrock: AWS shared-credentials profile
	Project         string            `toml:"project"`          // vertex: GCP project
	CredentialsFile string            `toml:"credentials_file"` // vertex: service account JSON
}

// Fallback is an ordered list of models to try when one cannot answer. When
// names the model it applies to as "[provider:]model"; Then is tried in order.
type Fallback struct {
	When string   `toml:"when"`
	Then []string `toml:"then"`
}

// Cost is the price per million tokens for a model prefix.
type Cost struct {
	In  float64 `toml:"in"`
	Out float64 `toml:"out"`
}

// Permissions are per-tool rules, each written as "tool" or "tool(subject)".
// They are checked before the permission mode: a deny holds even under bypass,
// an ask forces a prompt auto would have skipped, an allow skips one.
type Permissions struct {
	Allow []string `toml:"allow"`
	Ask   []string `toml:"ask"`
	Deny  []string `toml:"deny"`
}

// HookConfig is one lifecycle hook: a command run at a defined point around a
// tool call. Where it may come from is decided by TrustedHooks.
type HookConfig struct {
	Event   string `toml:"event"`   // pre_tool | post_tool | session_start | session_end
	Match   string `toml:"match"`   // glob over the tool name; empty matches every tool
	Command string `toml:"command"` // run with sh -c, like a bash call
	Timeout int    `toml:"timeout"` // seconds it may run; 0 = the built-in default
}

// MCPServerConfig describes one MCP server: a child process over stdio, or an
// HTTP endpoint when URL is set.
type MCPServerConfig struct {
	Name    string            `toml:"name"`
	Command string            `toml:"command"`
	Args    []string          `toml:"args"`
	Env     map[string]string `toml:"env"`
	URL     string            `toml:"url"`     // streamable HTTP endpoint
	Headers map[string]string `toml:"headers"` // sent on every request; ${VAR} expands
	Auth    string            `toml:"auth"`    // "oauth": log in through the browser
	// ClientID is only for an authorization server that refuses to register a
	// client on the spot; with dynamic registration there is nothing to fill in.
	ClientID string   `toml:"client_id"`
	Scopes   []string `toml:"scopes"` // oauth: overrides what the server advertises
}

// ExternalAgent is another agent Tapioca drives over the Agent Client
// Protocol: a subprocess speaking ACP on stdio, which gets a tab of its own.
//
//	[[agents.external]]
//	name    = "claude-code"
//	command = "claude"
//	args    = ["--acp"]
type ExternalAgent struct {
	Name    string            `toml:"name"`
	Command string            `toml:"command"`
	Args    []string          `toml:"args"`
	Env     map[string]string `toml:"env"`
}

// Agents groups what can be driven besides the models: an [agents] table
// rather than a top-level list, so later kinds have somewhere to go.
type Agents struct {
	External []ExternalAgent `toml:"external"`
}

// LSPServerConfig describes a language server used to check edited files.
type LSPServerConfig struct {
	Name       string   `toml:"name"`
	Command    string   `toml:"command"`
	Args       []string `toml:"args"`
	Extensions []string `toml:"extensions"`
}

// DashboardConfig controls the dashboard area.
type DashboardConfig struct {
	Visible  bool     `toml:"visible"`
	Panels   []string `toml:"panels"`
	Width    float64  `toml:"width"`    // fraction of the screen, 0.2 – 0.5
	Position string   `toml:"position"` // right | left | top | bottom
}

// Config is the root configuration. Settings changed inside the app are
// persisted back to the file via Save.
type Config struct {
	DefaultProvider string  `toml:"default_provider"`
	DefaultModel    string  `toml:"default_model"`
	SystemPrompt    string  `toml:"system_prompt"`
	MaxTokens       int     `toml:"max_tokens"`
	Temperature     float64 `toml:"temperature"`
	Thinking        bool    `toml:"thinking"`
	ThinkingBudget  int     `toml:"thinking_budget"`
	Verbose         bool    `toml:"verbose"` // full thoughts/tool output in chat
	Zen             bool    `toml:"zen"`     // hide keybind hints (/zen)
	Editor          string  `toml:"editor"`  // falls back to $VISUAL, $EDITOR, nvim, vim
	Autosave        bool    `toml:"autosave"`
	AutoCompact     bool    `toml:"auto_compact"`    // summarize when context nears the limit
	TitleModel      string  `toml:"title_model"`     // [provider:]model for session titles; empty uses the agent's
	BashTimeout     int     `toml:"bash_timeout"`    // seconds a tool call may run; 0 = 180
	ModelCatalog    bool    `toml:"model_catalog"`   // fetch model prices from models.dev at startup
	Sandbox         bool    `toml:"sandbox"`         // confine bash with bubblewrap
	SandboxNetwork  bool    `toml:"sandbox_network"` // allow network inside the sandbox
	Theme           string  `toml:"theme"`           // taro | contrast | mono
	Glyphs          string  `toml:"glyphs"`          // unicode | ascii | nerd
	Wordmark        string  `toml:"wordmark"`        // auto | compact | text | off
	PermissionMode  string  `toml:"permission_mode"` // plan | manual | auto | bypass

	BashAllow   []string                  `toml:"bash_allow"`  // always-allowed bash command words
	Permissions Permissions               `toml:"permissions"` // per-tool rules, checked before the mode
	SecretEnv   []string                  `toml:"secret_env"`  // extra env vars hidden from tools
	Hooks       []HookConfig              `toml:"hooks"`       // commands run around tool calls
	Providers   map[string]ProviderConfig `toml:"providers"`
	MCP         []MCPServerConfig         `toml:"mcp"`
	LSP         []LSPServerConfig         `toml:"lsp"`
	Agents      Agents                    `toml:"agents"`
	Dashboard   DashboardConfig           `toml:"dashboard"`
	Keys        map[string]string         `toml:"keys"`
	Colors      map[string]string         `toml:"colors"` // theme overrides: "#hex" or "#light/#dark"
	Costs       map[string]Cost           `toml:"costs"`  // model prefix -> $/Mtok
	Fallbacks   []Fallback                `toml:"fallback"`

	path    string // where this config was loaded from
	presave func(*Config)
	// unrestrict puts back what RestrictIfInsideTree refused to honour, so
	// changing a setting in the app does not delete keys from the file just
	// because this run declined to act on them.
	unrestrict func(*Config)
}

// SetPresave registers a transform applied to a copy of the config before
// every Save, so runtime-only overrides (CLI flags) never persist to disk.
func (c *Config) SetPresave(fn func(*Config)) { c.presave = fn }

// Presave returns that transform, so a reload — which replaces the whole
// config with a freshly loaded one — can carry it over. Without it the first
// save after a reload wrote the launch flags into the user's config file as
// settings: --sandbox as sandbox = true, and the servers --mcp-config named as
// the user's own [[mcp]].
func (c *Config) Presave() func(*Config) { return c.presave }

// Default returns the built-in configuration.
func Default() *Config {
	return &Config{
		DefaultProvider: "ollama",
		DefaultModel:    "",
		SystemPrompt: "You are a pragmatic coding assistant working in a terminal. " +
			"Use the available tools to read, write and edit files and to run shell commands " +
			"instead of describing what the user should do. Keep answers concise.",
		MaxTokens:      4096,
		Temperature:    1.0,
		Thinking:       false,
		ThinkingBudget: 2048,
		Verbose:        false,
		Autosave:       true,
		AutoCompact:    true,
		// "manual", not the legacy "ask": Load migrates that name away, so a
		// default of "ask" was a value the loader immediately rewrote — which
		// made a saved config differ from the default it was saved from.
		PermissionMode: "manual",
		// Sandboxing is opt-in, but once on, network access stays unless the
		// user turns it off — a silent loss of network reads as a broken build.
		SandboxNetwork: true,
		ModelCatalog:   true,
		BashTimeout:    180,
		Theme:          "taro",
		Glyphs:         "unicode",
		Wordmark:       "auto",
		// No llama.cpp entry, deliberately. Every configured provider is asked
		// for its models each time /model opens, and llama-server's port is
		// 8080 — the one a Spring Boot app, a Tomcat, a Jenkins or any dev
		// server takes first. Shipping the entry would send a request to
		// whatever that is and put its 404 on screen, for everyone who does not
		// run llama.cpp. /connect finds a real one instead, and adds it then.
		Providers: map[string]ProviderConfig{
			"ollama":    {Type: "ollama", BaseURL: "http://localhost:11434"},
			"anthropic": {Type: "anthropic", APIKeyEnv: "ANTHROPIC_API_KEY"},
		},
		Dashboard: DashboardConfig{
			Visible:  true,
			Panels:   []string{"agents", "tokens", "todos", "git", "tools", "settings"},
			Width:    0.33,
			Position: "right",
		},
		Keys: map[string]string{},
	}
}

// Dir returns the configuration directory.
func Dir() string {
	if d := os.Getenv("TAPIOCA_CONFIG_DIR"); d != "" {
		return d
	}
	if d := os.Getenv("XDG_CONFIG_HOME"); d != "" {
		return filepath.Join(d, "tapioca")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "tapioca")
}

// DataDir returns the directory for sessions and other state.
func DataDir() string {
	if d := os.Getenv("TAPIOCA_DATA_DIR"); d != "" {
		return d
	}
	if d := os.Getenv("XDG_DATA_HOME"); d != "" {
		return filepath.Join(d, "tapioca")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".local", "share", "tapioca")
}

// DefaultPath returns the default config file path.
func DefaultPath() string { return filepath.Join(Dir(), "config.toml") }

// Load reads the config at path (or the default path when empty), creating a
// commented default file on first run. Missing keys keep their defaults.
func Load(path string) (*Config, error) {
	if path == "" {
		path = DefaultPath()
	}
	cfg := Default()
	cfg.path = path
	if _, err := os.Stat(path); os.IsNotExist(err) {
		_ = WriteDefault(path) // best effort; the app works without a file
		return cfg, nil
	}
	// Decoding a table into a map merges into it rather than replacing it, so
	// the built-in providers outlived being deleted from the file: removing
	// [providers.ollama] left ollama configured and probed, and no edit to the
	// file could get rid of it. Cleared first, a [providers] table in the file
	// is the whole set — and a file that never mentions providers still falls
	// back to the defaults below, which is what leaves first run working.
	cfg.Providers = nil
	md, err := toml.DecodeFile(path, cfg)
	if err != nil {
		return nil, fmt.Errorf("parsing %s: %w", path, err)
	}
	def := Default()
	if cfg.MaxTokens <= 0 {
		cfg.MaxTokens = def.MaxTokens
	}
	if cfg.ThinkingBudget <= 0 {
		cfg.ThinkingBudget = def.ThinkingBudget
	}
	if cfg.Providers == nil {
		cfg.Providers = def.Providers
	}
	// An explicit "panels = []" means the user turned them all off, which is
	// different from never having mentioned panels at all.
	if !md.IsDefined("dashboard", "panels") {
		cfg.Dashboard.Panels = def.Dashboard.Panels
	}
	if cfg.Dashboard.Width < 0.2 || cfg.Dashboard.Width > 0.5 {
		cfg.Dashboard.Width = def.Dashboard.Width
	}
	if cfg.DefaultProvider == "" {
		cfg.DefaultProvider = def.DefaultProvider
	}
	cfg.EnsureDefaultProvider()
	if cfg.SystemPrompt == "" {
		cfg.SystemPrompt = def.SystemPrompt
	}
	if cfg.Keys == nil {
		cfg.Keys = map[string]string{}
	}
	switch cfg.PermissionMode { // migrate legacy names
	case "plan", "manual", "auto", "bypass":
	case "readonly":
		cfg.PermissionMode = "plan"
	default:
		cfg.PermissionMode = "manual"
	}
	switch cfg.Dashboard.Position {
	case "left", "top", "bottom":
	default:
		cfg.Dashboard.Position = "right"
	}
	cfg.path = path
	return cfg, nil
}

// EnsureDefaultProvider points default_provider at a provider that exists.
//
// Providers can be removed — from the file, or from /connect — and the default
// names "ollama" without anyone having written that down, so deleting the
// ollama entry and nothing else would leave every new agent pointed at a
// provider the config no longer has. Sorted, so which one it lands on does not
// depend on map order. A default that still resolves is never moved.
func (c *Config) EnsureDefaultProvider() {
	if _, ok := c.Providers[c.DefaultProvider]; ok || len(c.Providers) == 0 {
		return
	}
	names := make([]string, 0, len(c.Providers))
	for name := range c.Providers {
		names = append(names, name)
	}
	sort.Strings(names)
	c.DefaultProvider = names[0]
}

// Path returns where this config lives on disk.
func (c *Config) Path() string {
	if c.path == "" {
		return DefaultPath()
	}
	return c.path
}

// SecretEnvNames returns every environment variable this config turns into a
// credential: the ones named outright in secret_env, the api_key_env of each
// provider, and the ${VAR} an mcp header expands.
//
// A fixed table of well-known names in the secretenv package was not enough,
// and could not be: a variable holds a key because the config says to read it,
// not because someone thought of its name while writing that table. So
// api_key_env = "MY_GATEWAY_KEY" and an mcp header of "Bearer ${MCP_TOKEN}"
// went to every stdio MCP server, every language server and every bash call,
// which is exactly the exfiltration the scrubbing exists to stop.
func (c *Config) SecretEnvNames() []string {
	names := append([]string(nil), c.SecretEnv...)
	for _, p := range c.Providers {
		if p.APIKeyEnv != "" {
			names = append(names, p.APIKeyEnv)
		}
	}
	for _, s := range c.MCP {
		for _, v := range s.Headers {
			// os.Expand with a collecting function, so this reads exactly the
			// references the transport will substitute — no second parser to
			// drift from the first.
			os.Expand(v, func(name string) string {
				names = append(names, name)
				return ""
			})
		}
	}
	return names
}

// Save writes the current configuration back to the file it was loaded from.
// In-app settings changes call this, so users never have to edit the file by
// hand — and comments in the existing file are carried across, since a toggle
// should not cost you the documentation or your own notes.
func (c *Config) Save() error {
	path := c.path
	if path == "" {
		path = DefaultPath()
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tosave := *c
	if c.presave != nil {
		c.presave(&tosave)
	}
	// After presave, not before: the flag exclusions it applies were composed
	// from values this run had already restricted, so they must not win here.
	if c.unrestrict != nil {
		c.unrestrict(&tosave)
	}
	var buf bytes.Buffer
	if err := toml.NewEncoder(&buf).Encode(&tosave); err != nil {
		return err
	}
	// Write only what differs from the defaults. Load starts from Default()
	// and decodes over it, so an omitted key returns as the value that was
	// omitted — see minimize.go for why the rule cannot be "drop empties".
	var defBuf bytes.Buffer
	if err := toml.NewEncoder(&defBuf).Encode(Default()); err != nil {
		return err
	}
	// The existing file is read before minimizing, not after: minimize needs to
	// know which keys carry comments so it can keep them.
	existing, readErr := os.ReadFile(path)
	var comments commentMap
	if readErr == nil {
		comments = harvestComments(string(existing))
	}
	text := minimize(buf.String(), defBuf.String(), comments)
	if readErr == nil {
		text = applyComments(text, comments)
	} else {
		text = "# Tapioca configuration.\n" +
			"# Managed by the app (settings dashboard writes here); manual edits are\n" +
			"# fine too and are picked up on next start.\n\n" + text
	}
	return writeAtomic(path, []byte(text))
}

// writeAtomic writes data and renames it into place. A fixed "<path>.tmp" let
// two instances truncate each other's file and publish the result — and this
// is the file holding the provider keys, which the app refuses to start
// without. os.WriteFile also leaves an existing temp file's mode alone and
// follows a symlink sitting at that name.
func writeAtomic(path string, data []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp) // a no-op once the rename has moved it
	if err := f.Chmod(0o600); err != nil {
		f.Close()
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// WriteDefault writes a commented starter config to path.
func WriteDefault(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	// The annotated example goes beside the config rather than into it. It is
	// two hundred lines of documentation, and a config that opens with two
	// hundred lines of settings already at their defaults hides the two the
	// user actually changed — every one of them also had to be kept forever,
	// since a commented line is a line someone might have written.
	example := filepath.Join(filepath.Dir(path), exampleName)
	_ = os.WriteFile(example, []byte(defaultTOML), 0o600) // best effort
	return os.WriteFile(path, []byte(starterTOML), 0o600)
}

// exampleName is the annotated reference written next to the config.
const exampleName = "config.example.toml"

// starterTOML is a fresh config: nothing but a pointer to where the options
// are. Every setting has a default in code, so an empty file is a working one.
const starterTOML = `# Tapioca configuration.
#
# Everything has a default, so this file only needs the things you change.
# Settings changed in the app — tab to the dashboard, or /settings — are
# written back here, and only where they differ from the defaults.
#
# Every available option, with comments, is in ` + exampleName + ` beside
# this file. Copy the lines you want.
`

const defaultTOML = `# Tapioca configuration
# You rarely need to edit this by hand: focus the dashboard inside the app
# (tab) and change settings there — they are written back to this file.

default_provider = "ollama"     # which [providers.*] entry new agents use
default_model = ""              # empty = first model reported by the provider
max_tokens = 4096               # max output tokens per response
temperature = 1.0
thinking = false                # ask models to emit reasoning (or use /effort)
thinking_budget = 2048          # thinking token budget (anthropic)
verbose = false                 # show full thoughts and tool output in chat
zen = false                     # hide keybind hints; toggle with /zen
autosave = true                 # save the session after every completed turn
auto_compact = true             # summarize old turns when context nears the limit
permission_mode = "manual"      # plan | manual | auto | bypass (cycle with shift+tab)
sandbox = false                 # run bash inside bubblewrap: worktree writable,
                                # $HOME hidden, rest read-only (needs bwrap)
sandbox_network = true          # false cuts network inside the sandbox
bash_timeout = 180              # seconds a tool call may run; a bash call can
                                # ask for more (up to 30 min) via its timeout arg
model_catalog = true            # fetch model prices/context sizes from
                                # models.dev on startup; false = no network
# bash_allow = ["git", "go"]    # bash commands that never prompt ([p] in the prompt adds here)

# Per-tool permission rules, checked before the permission mode. Each is a
# tool name and, in parentheses, what the call is about: the path for file
# tools, the command for bash (per segment of a compound one), the URL for
# web_fetch, the arguments for an MCP tool. Paths glob with ** across
# directories, everything else with * over any run of characters.
# A deny holds in every mode, including bypass; an ask forces a prompt auto
# would have skipped; an allow skips one, except in plan mode.
# Matching is textual, so name the command, not one spelling of its flags:
# "bash(rm -rf*)" does not stop "rm -fr". See SECURITY.md.
# [permissions]
# allow = ["bash(go test*)", "edit_file(internal/**)"]
# ask   = ["bash(git push*)"]
# deny  = ["read_file(**/.env)", "bash(rm *)", "mcp:*__delete_*"]
# secret_env = ["MY_TOKEN"]     # extra env vars hidden from tools and MCP servers
                                # (provider API keys are always hidden)

# Hooks run a command of yours around tool calls. event is pre_tool,
# post_tool, session_start or session_end; match globs the tool name
# (mcp:server__tool for MCP tools) and covers every tool when omitted.
# The call is described in TAPIOCA_EVENT, TAPIOCA_TOOL, TAPIOCA_TOOL_PATH
# (file tools), TAPIOCA_TOOL_COMMAND (bash), TAPIOCA_TOOL_ERROR (post_tool)
# and TAPIOCA_CWD, with the exact arguments as JSON on stdin.
# A pre_tool hook that exits non-zero BLOCKS the call and its stderr is shown
# as the reason — including when it is missing or times out, so a policy that
# cannot run refuses rather than waves things through. A hook can only refuse:
# it never overrides a deny rule or skips a prompt. Provider keys are scrubbed
# from its environment. See SECURITY.md.
# [[hooks]]
# event = "post_tool"
# match = "edit_file"
# command = 'gofmt -w "$TAPIOCA_TOOL_PATH"'
# timeout = 30                  # seconds; 0 = 30

editor = ""                     # prompt editor; falls back to $VISUAL, $EDITOR, nvim, vim
# title_model = "ollama:qwen3"  # cheap model for session titles; empty = the agent's own

theme = "taro"                  # taro | contrast (colorblind-safe) | mono — /theme switches
glyphs = "unicode"              # unicode | ascii (any terminal) | nerd (patched font) — /glyphs
wordmark = "auto"               # welcome screen mark: auto (largest that fits) | compact |
                                # text (just the name) | off — /wordmark

# Override any theme color. "#hex" applies to both backgrounds, "#light/#dark"
# differs per background. Setting one lifts the mono theme's no-color rule.
# [colors]
# accent  = "#6C4FD8/#A78BFA"   # titles, focus, highlights
# dim     = "#8B8B98/#6A6A78"   # secondary text
# user    = "#1F8A5D/#6EE7A8"   # your messages
# error   = "#D03050/#FB7185"
# ok      = "#1F8A5D/#6EE7A8"
# warn    = "#B45309/#FBBF24"
# border  = "#D9D9E3/#33333E"
# think   = "#8E44AD/#D8B4FE"   # reasoning output
# tool    = "#1D6FB8/#7CC7FF"   # tool calls
# code_bg = "#F1F1F6/#232330"
# agents  = "#A78BFA, #7CC7FF, #5EEAD4"   # per-agent identity colors, cycled

system_prompt = """
You are a pragmatic coding assistant working in a terminal.
Use the available tools to read, write and edit files and to run shell
commands instead of describing what the user should do. Keep answers concise.
"""

[providers.ollama]
type = "ollama"
base_url = "http://localhost:11434"
# context_window = 8192         # used for the context gauge

# llama.cpp's llama-server. Uncomment to use it, or add it from /connect.
# Older builds need --jinja or tool calls are refused; recent ones have it on
# by default:
#   llama-server --jinja -m model.gguf
# [providers.llamacpp]
# type = "llamacpp"
# base_url = "http://localhost:8080"
# api_key_env = "LLAMA_API_KEY" # only if the server was started with --api-key

[providers.anthropic]
type = "anthropic"
api_key_env = "ANTHROPIC_API_KEY"
# api_key = "sk-ant-..."        # or put the key inline (not recommended)
# base_url = "https://api.anthropic.com"

# Models from every vendor behind one key, through the Vercel AI Gateway. The
# address is built in; model ids are vendor-qualified, e.g.
# "anthropic/claude-opus-5".
# [providers.vercel]
# type = "vercel"
# api_key_env = "AI_GATEWAY_API_KEY"

# Any server speaking the OpenAI API — a gateway, a proxy, a local model.
# auth_style says where the credential goes, since the same wire format is
# served behind all four of these.
# [providers.mygateway]
# type = "custom"
# base_url = "https://api.example.com/v1"
# api_key_env = "MY_GATEWAY_KEY"
# auth_style = "bearer"         # bearer | header | query | none
# auth_header = "X-API-Key"     # only for auth_style = "header"
# auth_query = "key"            # only for auth_style = "query"
#   [providers.mygateway.headers]   # sent on every request
#   X-Org-Id = "org_123"

# Anthropic models on AWS Bedrock. Credentials come from the usual AWS
# environment variables or ~/.aws/credentials; the model is a Bedrock id.
# [providers.bedrock]
# type = "bedrock"
# region = "us-east-1"
# profile = "default"           # optional: which ~/.aws/credentials profile

# Anthropic models on Google Vertex AI. Auth is a service account key, a
# GOOGLE_ACCESS_TOKEN, or whatever gcloud is logged in as.
# [providers.vertex]
# type = "vertex"
# project = "my-gcp-project"
# region = "us-east5"
# credentials_file = "/path/to/service-account.json"

# Google Gemini via its OpenAI-compatible endpoint.
# [providers.gemini]
# type = "gemini"
# api_key_env = "GEMINI_API_KEY"

# Azure OpenAI: base_url is your resource, and the model name is a deployment.
# [providers.azure]
# type = "azure"
# base_url = "https://<resource>.openai.azure.com"
# api_key_env = "AZURE_OPENAI_API_KEY"
# api_version = "2024-10-21"

# Any OpenAI-compatible server: OpenAI, LM Studio, vLLM, llama.cpp, OpenRouter…
# [providers.openai]
# type = "openai"
# api_key_env = "OPENAI_API_KEY"
# base_url = "https://api.openai.com"
# [providers.lmstudio]
# type = "openai"
# base_url = "http://localhost:1234"

# When a model cannot answer — rate limited, out of quota, provider erroring —
# try these instead, in order. A refusal or a bad request is an answer, not a
# failure, and is never retried elsewhere.
# [[fallback]]
# when = "anthropic:claude-opus-5"
# then = ["anthropic:claude-sonnet-5", "ollama:qwen3-coder"]

# Cost table for the dashboard, $ per million tokens, matched by model prefix.
# Sensible defaults exist for common anthropic/openai models; override here.
# [costs."claude-sonnet"]
# in = 3.0
# out = 15.0

[dashboard]
visible = true
width = 0.33                    # fraction of the screen used by dashboards
position = "right"              # right | left | top | bottom
# Available panels: agents, tokens, todos, git, changes, tools, mcp, session, settings
panels = ["agents", "tokens", "todos", "git", "tools", "settings"]

# Remote MCP servers over streamable HTTP. Headers are sent on every request;
# ${VAR} expands from the environment so tokens need not live in this file.
# [[mcp]]
# name = "example"
# url = "https://mcp.example.com/mcp"
# [mcp.headers]
# Authorization = "Bearer ${EXAMPLE_MCP_TOKEN}"

# A hosted server that wants an account rather than a token: auth = "oauth"
# logs in through the browser once (/mcp <name>) and refreshes on its own
# afterwards. The tokens are kept outside this file.
# [[mcp]]
# name = "linear"
# url = "https://mcp.linear.app/mcp"
# auth = "oauth"

# Language servers. After every edit the file is checked and any errors are
# attached to the tool result, so the agent fixes them straight away instead of
# finding out when a build runs. Servers start with the app.
# [[lsp]]
# name = "gopls"
# command = "gopls"
# extensions = [".go"]

# MCP servers (stdio transport). Their tools are offered to every agent.
# [[mcp]]
# name = "filesystem"
# command = "npx"
# args = ["-y", "@modelcontextprotocol/server-filesystem", "/tmp"]
#   [mcp.env]
#   EXAMPLE = "value"

# Other agents to drive over the Agent Client Protocol. /connect lists them;
# picking one gives it a tab, and its work streams into the transcript like any
# other agent's. Whatever it asks to run goes through the permission rules
# above — a deny still denies, even under bypass.
# [[agents.external]]
# name = "claude-code"
# command = "claude"
# args = ["--acp"]
#   [agents.external.env]
#   EXAMPLE = "value"

# Keybinds. Values are Bubble Tea key names; separate alternatives with
# commas; empty string unbinds. Press f1 in the app to see every action and
# its current binding. Rare operations are slash commands by default
# (/model, /new, /save, /resume, /panels, /systemprompt, /settings).
[keys]
# send = "enter"
# newline = "shift+enter,ctrl+j"
# quit = "ctrl+c"
# cancel = "esc"
# verbose = "ctrl+o"
# edit_prompt = "ctrl+g"
# toggle_thinking = "ctrl+t"
# cycle_mode = "shift+tab"
# new_agent = "ctrl+n"
# next_agent = "ctrl+right"
# prev_agent = "ctrl+left"
# close_agent = "ctrl+w"
# toggle_dashboard = "ctrl+b"
# move_panel_prev = "shift+up,shift+left"
# move_panel_next = "shift+down,shift+right"
# focus_next = "tab"
# help = "f1"
# copy_last = "y"
# copy_all = "Y"
`
