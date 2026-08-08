// Package config loads and writes Tapioca's TOML configuration.
package config

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"

	"github.com/BurntSushi/toml"
)

// ProviderConfig describes one LLM backend.
type ProviderConfig struct {
	Type          string `toml:"type"`           // "ollama" | "anthropic" | "openai"
	BaseURL       string `toml:"base_url"`       // optional override
	APIKey        string `toml:"api_key"`        // literal key (discouraged; prefer env)
	APIKeyEnv     string `toml:"api_key_env"`    // env var holding the key
	ContextWindow int    `toml:"context_window"` // tokens; 0 = per-type default
}

// Cost is the price per million tokens for a model prefix.
type Cost struct {
	In  float64 `toml:"in"`
	Out float64 `toml:"out"`
}

// MCPServerConfig describes one MCP server launched over stdio.
type MCPServerConfig struct {
	Name    string            `toml:"name"`
	Command string            `toml:"command"`
	Args    []string          `toml:"args"`
	Env     map[string]string `toml:"env"`
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
	PermissionMode  string  `toml:"permission_mode"` // plan | manual | auto | bypass

	BashAllow []string                  `toml:"bash_allow"` // always-allowed bash command words
	Providers map[string]ProviderConfig `toml:"providers"`
	MCP       []MCPServerConfig         `toml:"mcp"`
	Dashboard DashboardConfig           `toml:"dashboard"`
	Keys      map[string]string         `toml:"keys"`
	Costs     map[string]Cost           `toml:"costs"` // model prefix -> $/Mtok

	path    string // where this config was loaded from
	presave func(*Config)
}

// SetPresave registers a transform applied to a copy of the config before
// every Save, so runtime-only overrides (CLI flags) never persist to disk.
func (c *Config) SetPresave(fn func(*Config)) { c.presave = fn }

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
		PermissionMode: "ask",
		Providers: map[string]ProviderConfig{
			"ollama":    {Type: "ollama", BaseURL: "http://localhost:11434"},
			"anthropic": {Type: "anthropic", APIKeyEnv: "ANTHROPIC_API_KEY"},
		},
		Dashboard: DashboardConfig{
			Visible:  true,
			Panels:   []string{"agents", "tokens", "git", "tools", "settings"},
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
	if _, err := toml.DecodeFile(path, cfg); err != nil {
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
	if len(cfg.Dashboard.Panels) == 0 {
		cfg.Dashboard.Panels = def.Dashboard.Panels
	}
	if cfg.Dashboard.Width < 0.2 || cfg.Dashboard.Width > 0.5 {
		cfg.Dashboard.Width = def.Dashboard.Width
	}
	if cfg.DefaultProvider == "" {
		cfg.DefaultProvider = def.DefaultProvider
	}
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

// Path returns where this config lives on disk.
func (c *Config) Path() string {
	if c.path == "" {
		return DefaultPath()
	}
	return c.path
}

// Save writes the current configuration back to the file it was loaded from.
// In-app settings changes call this, so users never have to edit the file by
// hand. (Hand-written comments in the file are not preserved.)
func (c *Config) Save() error {
	path := c.path
	if path == "" {
		path = DefaultPath()
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tosave := *c
	if c.presave != nil {
		c.presave(&tosave)
	}
	var buf bytes.Buffer
	buf.WriteString("# Tapioca configuration.\n")
	buf.WriteString("# Managed by the app (settings dashboard writes here); manual edits are\n")
	buf.WriteString("# fine too and are picked up on next start.\n\n")
	if err := toml.NewEncoder(&buf).Encode(&tosave); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, buf.Bytes(), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// WriteDefault writes a commented starter config to path.
func WriteDefault(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(defaultTOML), 0o644)
}

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
# bash_allow = ["git", "go"]    # bash commands that never prompt ([p] in the prompt adds here)
editor = ""                     # prompt editor; falls back to $VISUAL, $EDITOR, nvim, vim

system_prompt = """
You are a pragmatic coding assistant working in a terminal.
Use the available tools to read, write and edit files and to run shell
commands instead of describing what the user should do. Keep answers concise.
"""

[providers.ollama]
type = "ollama"
base_url = "http://localhost:11434"
# context_window = 8192         # used for the context gauge

[providers.anthropic]
type = "anthropic"
api_key_env = "ANTHROPIC_API_KEY"
# api_key = "sk-ant-..."        # or put the key inline (not recommended)
# base_url = "https://api.anthropic.com"

# Any OpenAI-compatible server: OpenAI, LM Studio, vLLM, llama.cpp, OpenRouter…
# [providers.openai]
# type = "openai"
# api_key_env = "OPENAI_API_KEY"
# base_url = "https://api.openai.com"
# [providers.lmstudio]
# type = "openai"
# base_url = "http://localhost:1234"

# Cost table for the dashboard, $ per million tokens, matched by model prefix.
# Sensible defaults exist for common anthropic/openai models; override here.
# [costs."claude-sonnet"]
# in = 3.0
# out = 15.0

[dashboard]
visible = true
width = 0.33                    # fraction of the screen used by dashboards
position = "right"              # right | left | top | bottom
# Available panels: agents, tokens, git, changes, tools, mcp, session, settings
panels = ["agents", "tokens", "git", "tools", "settings"]

# MCP servers (stdio transport). Their tools are offered to every agent.
# [[mcp]]
# name = "filesystem"
# command = "npx"
# args = ["-y", "@modelcontextprotocol/server-filesystem", "/tmp"]
#   [mcp.env]
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
