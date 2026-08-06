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
          version = "0.1.0";
          src = ./.;
          vendorHash = "sha256-65pUwHJ6vbKrjcuki2l+1ma9x6u7EZ6YYkZsKtFNHUY=";
          subPackages = [ "." ];
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
