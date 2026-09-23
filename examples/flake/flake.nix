{
  description = "Minimal NixOS host installable with nixzner on Hetzner Cloud";

  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixos-26.05";
    disko.url = "github:nix-community/disko";
    disko.inputs.nixpkgs.follows = "nixpkgs";
  };

  outputs = { nixpkgs, disko, ... }:
    let
      mkHost = system: nixpkgs.lib.nixosSystem {
        modules = [
          disko.nixosModules.disko
          ./configuration.nix
          { nixpkgs.hostPlatform = system; }
        ];
      };
    in
    {
      nixosConfigurations = {
        example = mkHost "x86_64-linux"; # cx*/cpx*/ccx* server types
        example-arm = mkHost "aarch64-linux"; # cax* server types
      };
    };
}
