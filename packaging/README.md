# Packaging

`.github/workflows/publish.yml` runs when a release is **published** (not when
a tag is pushed — `release.yml` creates a draft first, and packages should
describe a release someone has looked at). `workflow_dispatch` re-runs one
version by hand.

Each publisher needs a credential this repository may not have. The `plan` job
turns each secret into a boolean and the rest skip cleanly, so you can connect
them one at a time without the run going red.

## What is actually automatable

Worth being blunt about, because the five differ enormously:

| Target | Who merges | Automated here |
|---|---|---|
| **Homebrew tap** | you | yes — pushes a formula on release |
| **AUR** | you | yes — the AUR *is* a git remote |
| **winget** | Microsoft's bot + a reviewer | opens the PR; merge is theirs |
| **apt** | you | builds `.deb`s, optionally hosts a signed repo |
| **nixpkgs** | nixpkgs reviewers | no — one manual PR, then a bot |

Only the first two publish the moment CI is green. winget lands when their
reviewers get to it. nixpkgs cannot be pushed to at all.

---

## Homebrew

Two different things share the name.

**A tap you own** works the day you create it, needs no permission, and is what
this workflow does.

1. Create a public repo called **`homebrew-tap`** under your account. The
   `homebrew-` prefix is what makes `brew tap juacamole/tap` work; the repo
   needs at least one commit (a README is fine).
2. Make a token that can push to it — either a classic PAT with `repo`, or
   (better) a fine-grained token scoped to just that repository with
   **Contents: read and write**.
3. Add it to this repo as the secret **`HOMEBREW_TAP_TOKEN`**
   (*Settings → Secrets and variables → Actions*).

Then:

```sh
brew install juacamole/tap/tapioca
```

The tap can be empty when you set the secret — the workflow creates `main`, a
README and `Formula/tapioca.rb` on its first run.

**The release has to be published first.** A draft release's assets are not
publicly downloadable, so a formula pointing at them fails for everyone until
you publish. The workflow only triggers on publish, so this orders itself, but
it is worth knowing if you ever push a formula by hand.

A `homebrew-test` job installs from the tap on a macOS runner afterwards and
runs both binaries — a formula can be valid Ruby and still fail to install, and
that check does not need brew on your own machine.

**homebrew-core** — `brew install tapioca` with no tap — is a different thing:
a manual PR to `Homebrew/homebrew-core`, subject to their
[acceptable formulae](https://docs.brew.sh/Acceptable-Formulae) rules, which
include a notability bar (roughly 30+ forks, 30+ watchers or 75+ stars). Submit
it once the project clears that; their CI takes over version bumps afterwards.
Nothing here can automate the first submission.

## AUR

The AUR is a git remote you push to, which makes it the most genuinely
automated of the five.

1. Make an account at <https://aur.archlinux.org>.
2. Generate a key **without a passphrase** (the action cannot type one):
   `ssh-keygen -t ed25519 -C aur -f ~/.ssh/aur` — add `~/.ssh/aur.pub` under
   *My Account → SSH Public Key*.
3. Add three secrets here: **`AUR_SSH_PRIVATE_KEY`** (the whole private key,
   including the BEGIN/END lines), **`AUR_USERNAME`**, **`AUR_EMAIL`**.
4. Check the name is free before the first run:
   `git ls-remote ssh://aur@aur.archlinux.org/tapioca-bin.git` — an empty
   result means it is yours to create, and the first push creates it.

`tapioca-bin`, not `tapioca`, is deliberate: the AUR convention is that an
unsuffixed name builds from source and `-bin` installs a published binary. A
from-source package would rebuild what the release already produced and tested.

```sh
yay -S tapioca-bin
```

## winget

The workflow opens a PR against `microsoft/winget-pkgs`; a bot validates the
manifest and a human merges it.

1. Fork <https://github.com/microsoft/winget-pkgs> to your account. The action
   pushes its branch there.
2. Create a **classic** PAT with the `public_repo` scope (fine-grained tokens
   do not work with this action) and add it as **`WINGET_TOKEN`**.

**The first submission usually has to be created by hand.** These are portable
binaries in a zip, so the manifest needs `NestedInstallerType: portable` with
one entry per binary, which the releaser action updates but does not invent:

```powershell
winget install wingetcreate
wingetcreate new https://github.com/juacamole/tapioca/releases/download/v1.0.0/tapioca-v1.0.0-windows-amd64.zip
```

Use the identifier **`Juacamole.Tapioca`** — it must match `identifier:` in the
workflow — and declare both `tapioca.exe` and `tapio.exe` as portable
commands. After that PR merges, this workflow keeps it current.

## apt

There is no central apt registry. Debian and Ubuntu proper need a Debian
Developer to sponsor an upload, which is a months-long social process, not a
CI step. Two things are real, and the workflow does both:

**`.deb` files on the release.** No secrets, always runs. Built with `nfpm`
from the binaries the release already produced, so what is packaged is what was
tested rather than a rebuild.

```sh
curl -LO https://github.com/juacamole/tapioca/releases/download/v1.0.0/tapioca_1.0.0_amd64.deb
sudo dpkg -i tapioca_1.0.0_amd64.deb
```

**A signed apt repository**, published to an `apt` branch of this repo and
served by GitHub Pages, so `apt update` works. To turn it on:

1. Make a signing key — use a dedicated one, not your personal key:
   ```sh
   gpg --quick-generate-key "Tapioca Packages <you@example.com>" default default never
   gpg --armor --export-secret-keys <KEYID>
   ```
2. Add **`GPG_PRIVATE_KEY`** (that armored block) and **`GPG_PASSPHRASE`**.
3. After the first run, point Pages at the `apt` branch
   (*Settings → Pages → Branch: apt, folder: /*).

Users then:

```sh
curl -fsSL https://juacamole.github.io/tapioca/tapioca.gpg \
  | sudo tee /etc/apt/keyrings/tapioca.gpg > /dev/null
echo "deb [signed-by=/etc/apt/keyrings/tapioca.gpg] https://juacamole.github.io/tapioca stable main" \
  | sudo tee /etc/apt/sources.list.d/tapioca.list
sudo apt update && sudo apt install tapioca
```

A **PPA** on Launchpad is the other option for Ubuntu specifically. It needs a
Launchpad account, a GPG key registered there, and *source* packages rather
than binaries — a different build entirely, which is why it is not wired up
here.

## nixpkgs

Cannot be automated, by design: nixpkgs is a reviewed pull request against
`NixOS/nixpkgs`.

Until it lands, the flake in this repository is the supported route and needs
nothing set up:

```sh
nix run github:juacamole/tapioca
nix profile install github:juacamole/tapioca
```

To submit it, copy `packaging/nix/package.nix` to
`pkgs/by-name/ta/tapioca/package.nix` in a nixpkgs checkout, fill in the source
hash (the `nix` job in the workflow prints it, or run
`nix-prefetch-url --unpack <tarball>`), add yourself to `maintainers`, and open
a PR. After the first one merges, `r-ryantm` proposes version bumps
automatically and this file only needs touching when the build itself changes.

The `nix` job in the workflow does not publish anything — it proves the flake
still builds from the tag and prints the numbers that PR needs.

---

## Adding a new release

Merging to `main` publishes nothing. The chain is:

```
dev  →  main  →  git tag v1.2.3  →  draft release  →  you publish it  →  packages
```

`git tag v1.2.3 && git push origin v1.2.3` builds the release as a draft — the
workflow refuses a tag that is not on `main` — and publishing that draft is
what triggers everything above.

If a publisher fails, re-run just that one with *Actions → publish packages →
Run workflow* and the tag — every job is idempotent and will no-op if the
package is already current.
