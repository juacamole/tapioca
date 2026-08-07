# Contributing workflow

Every change follows the same loop, whether human- or agent-authored.

## Issues first

GitHub issues are the roadmap. Feature and fix work starts from an issue;
if none exists, file one. Small chores (typo, CI tweak) may skip this.

## Branches

Named after the issue: `feat/14-subagents`, `fix/20-paste`. Type prefixes:
`feat/`, `fix/`, `docs/`, `chore/`.

## Commits

Conventional commits, issue id in the title:

```
feat(agent): subagents for task delegation (#14)
```

Body explains the mechanism, not the diff. No AI co-author trailers.
Comments in code stay minimal — only constraints the code can't express.

## Pull requests

Work merges through PRs, not local merges:

```sh
git push -u origin feat/14-subagents
gh pr create --fill --body "Closes #14"
gh pr merge --merge          # merge commit, keeps branch history visible
git checkout main && git pull
```

`Closes #N` in the PR body auto-closes the issue on merge.

## Quality bar

`gofmt -l .` clean, `go vet ./...` clean, `go build ./...` green (CI checks
all three). New behavior gets exercised against a mock or in a live tmux
session before merging.
