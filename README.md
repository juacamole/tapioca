# Tapioca

An agentic coding TUI in the spirit of Claude Code, Crush and OpenCode, built
with [Bubble Tea](https://github.com/charmbracelet/bubbletea).

Chat fills two thirds of the screen; the right third is a stack of
**configurable dashboards** (agents, tokens/context/cost, tool calls, MCP
servers, session, editable settings). Agents can read, write and edit files
and run shell commands — every mutating call goes through a permission
prompt.

```
┌ title: session · cwd · provider:model · effort · max tokens ──────────┐
│ ● 1·agent-1   ● 2·agent-2                                             │
│ ╭─ chat (2/3) ─────────────────────────────╮ ╭─ dashboards (1/3) ───╮ │
│ │ | you                                    │ │ agents               │ │
│ │ fix the failing parser test              │ │ tokens  ctx ██░ 41%  │ │
│ │                                          │ │         cost $0.0214 │ │
│ │ | agent-1 · claude-sonnet-5              │ │ tool calls           │ │
│ │ tool: bash go test ./parser/...          │ │ settings · editing   │ │
│ │ bash -> ok                               │ ╰──────────────────────╯ │
│ │ ──────────────────────────────────────── │                          │
│ │ | /diff                                  │                          │
│ ╰──────────────────────────────────────────╯                          │
│ status: hints / flash messages · agent status · tok/s                 │
└───────────────────────────────────────────────────────────────────────┘
```

## What it does

- **Built-in coding tools** — `bash`, `read_file`, `write_file`, `edit_file`
  run in the working directory (`/cd` to move it), plus keyless
  **`web_search`** (DuckDuckGo) and **`web_fetch`** (readable page text) for
  research; the web tools are read-only, need no permission, and work in
  plan mode.
- **Permission modes** — cycle with `shift+tab`, shown in the status bar:
  - `plan` — no file modifications; bash asks; the agent is instructed to
    investigate read-only and present a plan
  - `manual` — every mutating tool call prompts (allow once / always / deny)
  - `auto` — file edits auto-approved, bash still asks
  - `bypass` — everything runs without asking
- **Providers** — Ollama, Anthropic, and any **OpenAI-compatible** server
  (LM Studio, vLLM, llama.cpp, OpenRouter, OpenAI) via `type = "openai"`.
  All stream, all support tool calls and thinking where the backend does.
- **MCP** — stdio servers from config; their tools are namespaced
  `server__tool` and offered alongside the built-ins.
- **Multi-agent** — independent agents with their own provider, model,
  system prompt, goal, history and stats; they stream concurrently.
  `/fork` branches the current conversation into a new agent.
- **Conversation mechanics** — `/regen` regenerates the last response,
  `/edit` pulls the last prompt back into the input, `up` recalls prompt
  history, and prompts typed while the agent is busy are **queued** and sent
  automatically when it finishes.
- **Context & cost visibility** — the tokens panel shows a context-fill
  gauge (window size per provider, `context_window` to override) and an
  estimated session cost from a configurable `[costs]` price table.
  `/compact` summarizes the conversation to free context.
- **Dashboards** — focus with `tab`, edit settings in place (model picker,
  permission mode, max tokens, temperature, thinking, budget, tools,
  verbose, dashboard side); every change is applied live and **written back
  to the config file**. The dashboard can dock **right, left, top or
  bottom** (`position` in config or the "dash side" setting), and panels are
  reordered with left/right inside the panel picker (`p`).
- **Git panels** — a `git` panel (branch, ahead/behind, last commit, change
  counts) and a `changes` panel (IntelliJ/VSCode-style file list with
  staged/unstaged/untracked coloring), refreshed automatically and after
  every agent turn; `/diff` shows the full diff.
- **Copy & selection** — drag with the mouse over the chat to mark text; it
  is copied on release (wl-copy/xclip/pbcopy, OSC 52 fallback). In chat
  focus `y` copies the last response and `Y` the whole transcript. `f3`
  releases the mouse entirely for native terminal selection.
- **/settings** — opens the config file in vim/$EDITOR and hot-reloads it on
  save (keybinds, providers, defaults, layout — applied to running agents).
- **Sessions** — the whole workspace autosaves; `/resume` opens a picker
  whose filter searches **across the text of all saved conversations**.
- **Vim everywhere** — `ctrl+e` edits the prompt, `ctrl+g` the system
  prompt, in `$EDITOR`/nvim/vim. Verbose vs compact transcripts on `ctrl+o`.

## Slash commands

Type `/` for the completion popup:

| Command | Effect |
|---|---|
| `/model [provider:]name` | switch model (no arg opens the picker) |
| `/effort off\|low\|medium\|high` | set thinking effort |
| `/goal text` · `/goal clear` | pin a goal into the system prompt |
| `/btw note` | add context without asking for a reply |
| `/cd dir` | change the working directory |
| `/diff` | scrollable git diff (staged + unstaged) |
| `/clear` | clear this agent's conversation |
| `/compact` | summarize the conversation to free context |
| `/regen` · `/edit` | redo / rewrite the last exchange |
| `/fork` · `/agent [name]` | branch conversation / new agent |
| `/resume [id]` · `/new` · `/save` | session management |
| `/verbose` · `/thinking` · `/tools` · `/zen` | toggles |
| `/help` · `/quit` | the obvious |

## Keybinds (defaults, no alt — mac friendly)

| Action | Keys | | Action | Keys |
|---|---|---|---|---|
| send | `enter` | | new agent | `ctrl+n` |
| newline | `shift+enter` (`ctrl+j`) | | next / prev agent | `ctrl+→` / `ctrl+←` |
| stop · clear · quit (2x) | `ctrl+c` | | close agent | `ctrl+w` |
| cancel / close overlay | `esc` | | toggle dashboards | `ctrl+b` |
| cycle permission mode | `shift+tab` | | cycle focus | `tab` (or click) |
| verbose chat | `ctrl+o` | | move focused panel | `shift+arrows` |
| edit prompt in vim | `ctrl+g` | | help | `f1` |
| toggle thinking | `ctrl+t` | | |

Sessions, model picking, the system prompt and panel management live in
slash commands: `/new`, `/save`, `/resume`, `/model`, `/panels`,
`/systemprompt`, `/settings`.

**Marking text in the chat copies it automatically** — drag with the mouse,
and the moment you let go it is on the clipboard (the mark stays highlighted
until the next click). In chat focus: `j/k` scroll, `u/d` page, `g/G`
top/bottom, `y`/`Y` copy last response/transcript. Clicking a pane focuses
it (the chat drops you into write mode); clicking a dashboard panel selects
it, and `enter` on the settings panel edits its rows.

`ctrl+c` behaves like Claude Code: one press stops a running generation;
idle it clears the input; a second press within two seconds exits (with
autosave). Everything is rebindable under `[keys]`.

**shift+enter**: most terminals cannot transmit shift+enter distinctly —
same limitation Claude Code solves with `/terminal-setup`. Map it to a
literal newline in your terminal and Tapioca picks it up (it equals
`ctrl+j`). Kitty: `map shift+enter send_text all \n` in kitty.conf.
Otherwise `ctrl+j` always works.

## Build & run

With nix (flake):

```sh
nix run .                          # run straight from the repo
nix build .                        # binary at ./result/bin/tapioca
nix develop                        # dev shell: go, gopls, gh, tmux, python3
nix profile install .              # install onto your PATH
```

Without nix:

```sh
go build -o tapioca .              # build (needs the repo dir for go.mod)
cp tapioca ~/.npm-global/bin/      # or any directory on your PATH
tapioca                            # then run from anywhere
tapioca -r                         # resume the most recent session
tapioca -list                      # list saved sessions
tapioca -session ID
```

Requires Go 1.22+ to build. `go run .`/`go build` must run inside the repo
(Go needs `go.mod`); the compiled binary has no such requirement — install
it on your PATH and start it from any project directory, which becomes the
agent's working directory.

With Ollama running locally it works out of the box.

## Configuration

`~/.config/tapioca/config.toml` — created on first run and **kept up to date
by the app** (settings dashboard, model picker, toggles and the system
prompt editor all write back). Sessions live in
`~/.local/share/tapioca/sessions/`. Things typically set once by hand:

```toml
permission_mode = "ask"            # ask | auto | readonly

[providers.lmstudio]
type = "openai"                    # any OpenAI-compatible server
base_url = "http://localhost:1234"
context_window = 32768

[[mcp]]
name = "filesystem"
command = "npx"
args = ["-y", "@modelcontextprotocol/server-filesystem", "/tmp"]

[costs."claude-sonnet"]            # $ per Mtok, matched by model prefix
in = 3.0
out = 15.0

[keys]
save_session = "ctrl+s,f5"
```

## Architecture

```
main.go                  flags, config, executor cwd, session resume
internal/config          TOML config: load, defaults, Save (written by the app)
internal/provider        Provider interface; ollama (NDJSON), anthropic (SSE),
                         openai-compatible (SSE)
internal/tools           built-in bash/read/write/edit tools + permission gate
internal/mcp             stdio JSON-RPC 2.0 MCP client + tool registry
internal/agent           agent runtime (stream → tool loop → events), manager,
                         fork, permission events
internal/session         workspace snapshots as JSON + full-text search blobs
internal/stats           token / request / tool-call accounting
internal/ui              Bubble Tea app: chat, dashboards, slash commands,
                         pickers, permission & diff overlays, vim integration
```

Each agent streams in its own goroutine and reports through a channel; the
UI consumes events so several agents can generate at once. Permission
requests travel the same channel and block the agent until answered.
