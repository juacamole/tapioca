# The expression for a nixpkgs submission, as pkgs/by-name/ta/tapioca/package.nix.
#
# This is not built by CI: nixpkgs is a reviewed pull request against
# NixOS/nixpkgs, not a registry that can be pushed to. Until it lands, the
# flake at the repository root is the supported way to get this with Nix:
#
#   nix run github:juacamole/tapioca
#   nix profile install github:juacamole/tapioca
#
# After the first PR lands, r-ryantm proposes version bumps automatically and
# this file only needs touching when the build itself changes.
{
  lib,
  buildGoModule,
  fetchFromGitHub,
  installShellFiles,
}:

buildGoModule rec {
  pname = "tapioca";
  version = "1.0.0";

  src = fetchFromGitHub {
    owner = "juacamole";
    repo = "tapioca";
    rev = "v${version}";
    # nix-prefetch-url --unpack https://github.com/juacamole/tapioca/archive/refs/tags/v1.0.0.tar.gz
    hash = "sha256-AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=";
  };

  # Must match flake.nix; the publish workflow prints the current value.
  vendorHash = "sha256-oeW8Am7NCkHGlTj2M0gBQqQzxKAGqxgIWLaZFIpY+Pc=";

  # Both names: Shopify's tapioca gem installs a `tapioca` too.
  subPackages = [
    "."
    "cmd/tapio"
  ];

  ldflags = [
    "-s"
    "-w"
  ];

  nativeBuildInputs = [ installShellFiles ];

  # The test suite shells out to git and sh and writes under $HOME.
  preCheck = ''
    export HOME=$(mktemp -d)
  '';

  meta = {
    description = "Agentic coding TUI for local and hosted LLMs";
    homepage = "https://github.com/juacamole/tapioca";
    changelog = "https://github.com/juacamole/tapioca/releases/tag/v${version}";
    license = lib.licenses.mit;
    mainProgram = "tapioca";
    maintainers = [ ]; # add yourself: maintainers/maintainer-list.nix
    platforms = lib.platforms.unix;
  };
}
