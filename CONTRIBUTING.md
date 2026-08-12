# Contributing

Thanks for taking a look. This is a small project with a simple workflow —
nothing here should surprise you.

## Getting set up

You need **Go 1.26+**. Everything else is optional.

```sh
git clone https://github.com/juacamole/tapioca
cd tapioca
go build ./...
go test ./...
./tapioca
```

With [Nix](https://nixos.org), `nix develop` gives you the toolchain and
`nix build` produces the binary. If you use direnv, `direnv allow` picks up
the flake automatically.

On first run Tapioca writes a commented config to
`~/.config/tapioca/config.toml` and points itself at a local Ollama. Any
OpenAI-compatible server works too — see the README.

## Branches

`main` is always releasable and is what people see. `dev` is where work is
integrated. Feature branches come off `dev` and merge back into it; `dev`
merges into `main` when you want main to move.

```
feature/  →  dev  →  main  →  tag  →  draft release  →  publish  →  packages
```

Nothing after `main` happens on its own. Tagging builds a **draft** release,
publishing that draft makes its assets public, and packaging is then started by
hand (*Actions → publish packages*) — see [packaging](packaging/README.md) for
why that last step cannot fire itself. None of them is a side effect of
merging. CI runs on both
branches and on every pull request into either.

A release must be tagged from `main`: the release workflow refuses a tag whose
commit is not an ancestor of `main`, because a published release cannot be
withdrawn from the package managers that have already copied it.

## Making a change

**Start from an issue.** The issue list is the roadmap; if what you want to do
isn't there, open one first so we can agree on the shape before you write
code. Typos and CI tweaks can skip this.

Branch names carry the issue number, so it's obvious later what a branch was
for:

```
feat/14-subagents
fix/20-paste-crash
docs/31-provider-setup
perf/88-session-index
```

Commit messages are [conventional commits](https://www.conventionalcommits.org)
with the issue number in the subject:

```
feat(agent): subagents for task delegation (#14)
```

Write the body for someone reading `git log` in a year. Explain **why** the
change is the way it is, and anything you tried that didn't work — the diff
already says what changed.

## Pull requests

Changes land through PRs so there's a place to discuss them:

```sh
git push -u origin feat/14-subagents
gh pr create --body "Closes #14"
gh pr merge --merge --delete-branch
```

`Closes #14` in the body closes the issue when the PR merges. If a PR closes
more than one issue, repeat the keyword — `Closes #14, closes #15` — because
GitHub only acts on the first one otherwise.

## What gets a change merged

CI runs `gofmt`, `go vet`, `go build`, `go test -race`, staticcheck and
govulncheck, and cross-compiles for Windows and macOS. Green CI is the floor,
not the bar. Beyond that:

**Tests that would fail without your change.** The habit that catches the most
here is writing the test first, watching it fail for the right reason, then
fixing it. A test that passes before and after is telling you nothing. If you
can't test something automatically — most of the TUI — say in the PR how you
exercised it by hand.

**Measure before optimizing.** Several "obvious" performance fixes in this
repo turned out to target things costing 0.1 ms. If you're making something
faster, put the numbers in the PR.

**Comments explain constraints, not mechanics.** Don't narrate what the code
does; say why it has to be that way, what breaks otherwise, or what you tried
first. Most functions need none.

**Security-relevant changes need a threat model.** Anything touching the
permission gate, the sandbox, credential handling, or parsing of untrusted
input: say in the PR who the attacker is and what stops them.
[SECURITY.md](SECURITY.md) documents what is actually enforced today, and it
should never promise more than the code delivers.

## Layout

```
main.go, cmd/tapio/   two names for one binary (see internal/cli)
internal/cli/         entry point, flag parsing, headless mode
internal/agent/       the agent loop, subagents, todo/plan handling
internal/provider/    one file per backend (anthropic, openai, ollama, …)
internal/tools/       built-in tools and the permission gate
internal/ui/          Bubble Tea model, rendering, dashboard panels
internal/session/     saving, loading and indexing conversations
internal/mcp/         MCP client (stdio and streamable HTTP)
internal/lsp/         language-server client for post-edit diagnostics
internal/acp/         editor integration over the Agent Client Protocol
```

`scripts/acp-probe.sh` drives `--acp` the way an editor would, so you can test
editor integration without an editor.

## Reporting a bug

Include the Tapioca version, your OS, the provider and model, and what you
expected instead. If it involves a permission prompt or a tool call, the exact
command or path matters — those paths are full of edge cases.

For anything with security impact, please don't open a public issue; see
[SECURITY.md](SECURITY.md).

## A note on AI-assisted contributions

Much of this codebase was written with AI assistance, and yours can be too.
The bar is the same either way: you understand the change, you can explain why
it works, and you've verified it does. Don't send a patch you can't defend in
review. Please leave AI co-author trailers out of commits — the author is
whoever takes responsibility for the code.
