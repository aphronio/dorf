{ version }:
let
  pins = builtins.fromJSON (builtins.readFile ./packages.json);
  pkgs = import (builtins.fetchTarball {
    url = "https://github.com/NixOS/nixpkgs/archive/${pins.nixpkgs.revision}.tar.gz";
    sha256 = pins.nixpkgs.sha256;
  }) { system = "x86_64-linux"; };
  source = pins.codex.${version};
in pkgs.stdenvNoCC.mkDerivation {
  pname = "dorf-codex";
  inherit version;
  src = pkgs.fetchurl { inherit (source) url hash; };
  dontConfigure = true;
  dontBuild = true;
  dontFixup = true;
  installPhase = ''
    runHook preInstall
    mkdir -p "$out"
    cp -a vendor/x86_64-unknown-linux-musl/. "$out/"
    runHook postInstall
  '';
}
