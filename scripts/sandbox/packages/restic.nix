{ pkgs }:

let
  pins = (builtins.fromJSON (builtins.readFile ./packages.json)).restic;
in
pkgs.buildGoModule {
  pname = "restic";
  inherit (pins) version;
  src = pkgs.fetchFromGitHub {
    owner = "restic";
    repo = "restic";
    rev = pins.revision;
    hash = pins.source_hash;
  };
  vendorHash = pins.vendor_hash;
  subPackages = [ "cmd/restic" ];
  doCheck = false;
}
