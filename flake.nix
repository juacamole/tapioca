{
  description = "Tapioca — agentic coding TUI for local and hosted LLMs";

  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixpkgs-unstable";
    flake-utils.url = "github:numtide/flake-utils";
  };

  outputs = { self, nixpkgs, flake-utils }:
    flake-utils.lib.eachDefaultSystem (system:
      let
        pkgs = nixpkgs.legacyPackages.${system};
      in
      {
        packages.default = pkgs.buildGoModule {
          pname = "tapioca";
          version = "1.2.0"; # kept in step with internal/version by CI
          src = ./.;
          vendorHash = "sha256-2Kqk4C+Ovy0wDSTVB/IHv+y3bDhCadontTP3GZ7a8/M=";
          # Both names, one program: Shopify's tapioca gem installs a
          # `tapioca` binary too, so `tapio` is there when that one wins PATH.
          subPackages = [ "." "cmd/tapio" ];
          meta = {
            description = "Agentic coding TUI for local and hosted LLMs";
            mainProgram = "tapioca";
          };
        };

        apps.default = {
          type = "app";
          program = "${self.packages.${system}.default}/bin/tapioca";
        };

        devShells.default = pkgs.mkShell {
          packages = with pkgs; [ go gopls gh git python3 tmux ];
        };
      });
}
