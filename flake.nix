{
  description = "herald — self-hosted agent report gateway";

  inputs.nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";

  outputs = { self, nixpkgs }:
    let
      systems = [ "x86_64-linux" "aarch64-linux" "x86_64-darwin" "aarch64-darwin" ];
      forAllSystems = f: nixpkgs.lib.genAttrs systems (system: f (import nixpkgs { inherit system; }));
    in
    {
      packages = forAllSystems (pkgs: rec {
        herald = (pkgs.buildGoModule.override { go = pkgs.go_1_27; }) {
          pname = "herald";
          version = "0.1.0";
          src = self;
          subPackages = [ "cmd/herald" ];
          vendorHash = "sha256-7moMtarOGbrMEB8iWCtAiAZXPQxMojXLevg1jk54hog=";
          ldflags = [ "-s" "-w" ];
          meta = with pkgs.lib; {
            description = "Self-hosted agent report gateway";
            mainProgram = "herald";
            license = licenses.mit;
          };
        };
        default = herald;
      });

      devShells = forAllSystems (pkgs: {
        default = pkgs.mkShell {
          packages = [ pkgs.go pkgs.golangci-lint pkgs.postgresql ];
        };
      });

      nixosModules.default = import ./nix/module.nix;
      nixosModules.herald = import ./nix/module.nix;
    };
}
