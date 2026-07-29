{
  description = "hoard — central restic backup server + desktop agent";

  inputs = {
    nixpkgs.url = "github:nixos/nixpkgs/nixos-unstable";
    flake-utils.url = "github:numtide/flake-utils";
  };

  outputs = { self, nixpkgs, flake-utils }:
    flake-utils.lib.eachDefaultSystem (system:
      let
        pkgs = import nixpkgs { inherit system; };
        version = "0.1.0";
        # No external Go modules, so nothing to vendor.
        mkBin = name: pkgs.buildGoModule {
          pname = name;
          inherit version;
          src = ./.;
          vendorHash = null;
          subPackages = [ "cmd/${name}" ];
          ldflags = [ "-s" "-w" ];
          meta = with pkgs.lib; {
            description = "hoard ${name}";
            homepage = "https://github.com/nauski/hoard";
            license = licenses.mit;
            mainProgram = name;
          };
        };
      in {
        packages = {
          hoardd = mkBin "hoardd";
          hoard-agent = mkBin "hoard-agent";
          default = self.packages.${system}.hoard-agent;
        };

        # `nix run .#hoard-agent`
        apps.hoard-agent = {
          type = "app";
          program = "${self.packages.${system}.hoard-agent}/bin/hoard-agent";
        };
      })
    // {
      # Home-manager module for the desktop agent. Import and set
      # `services.hoard-agent.repository`; secrets come from a password file
      # (e.g. sops-nix), never from the Nix store.
      homeManagerModules.hoard-agent = import ./nix/hm-module.nix self;
    };
}
