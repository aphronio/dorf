let pins = builtins.fromJSON (builtins.readFile ./packages.json);
in import (builtins.fetchTarball {
  url = "https://github.com/NixOS/nixpkgs/archive/${pins.nixpkgs.revision}.tar.gz";
  sha256 = pins.nixpkgs.sha256;
}) { system = "x86_64-linux"; }
