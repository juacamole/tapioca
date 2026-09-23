# The expression for a nixpkgs submission, as pkgs/by-name/ta/tapioca/package.nix.
#
# This is the file that was submitted as NixOS/nixpkgs#566153. It passed their
# CI — nixpkgs-vet, eval on all three platforms, and the build — and was
# declined on the userbase policy rather than on anything here, so it is kept
# ready for the day that changes.
#
# What actually ships today is NUR, which the release workflow updates from
# this shape: github.com/juacamole/nur-packages. The two differ in one line,
# because lib.maintainers.juacamole only exists inside that nixpkgs PR.
#
# The flake at the repository root remains the no-setup route:
#
#   nix run github:juacamole/tapioca
#   nix profile install github:juacamole/tapioca
{
  lib,
  buildGoModule,
  fetchFromGitHub,
  git,
}:

buildGoModule (finalAttrs: {
  __structuredAttrs = true;

  pname = "tapioca";
  version = "1.2.0";

  src = fetchFromGitHub {
    owner = "juacamole";
    repo = "tapioca";
    tag = "v${finalAttrs.version}";
    hash = "sha256-NcDSk0N7r4UWTM+VhqulCmkXE6LfRvBFoNmwVufAaZU=";
  };

  vendorHash = "sha256-2Kqk4C+Ovy0wDSTVB/IHv+y3bDhCadontTP3GZ7a8/M=";

  # Installs under both names: Shopify's tapioca gem provides a `tapioca`
  # binary too, so upstream ships `tapio` for when that one wins PATH.

  nativeCheckInputs = [ git ];

  # The suite drives real git repositories and writes under $HOME.
  preCheck = ''
    export HOME=$(mktemp -d)
  '';

  ldflags = [
    "-s"
    "-w"
  ];

  meta = {
    description = "Agentic coding TUI for local and hosted LLMs";
    homepage = "https://github.com/juacamole/tapioca";
    changelog = "https://github.com/juacamole/tapioca/releases/tag/v${finalAttrs.version}";
    license = lib.licenses.mit;
    mainProgram = "tapioca";
    maintainers = with lib.maintainers; [ juacamole ];
    platforms = lib.platforms.unix;
  };
})
