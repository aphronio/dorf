{ pkgs ? import ./nixpkgs.nix }:
let pins = (builtins.fromJSON (builtins.readFile ./packages.json)).pi;
in pkgs.buildNpmPackage {
  pname = "dorf-pi";
  inherit (pins) version;
  src = ./pi;
  npmDepsHash = pins.npm_deps_hash;
  npmDepsFetcherVersion = 2;
  nodejs = pkgs.nodejs_24;
  dontNpmBuild = true;
  npmFlags = [ "--omit=dev" ];
  npmRebuildFlags = [ "--ignore-scripts" ];
  nativeBuildInputs = [ pkgs.makeWrapper ];
  installPhase = ''
    mkdir -p "$out/lib"
    cp -r node_modules "$out/lib/"
    makeWrapper ${pkgs.nodejs_24}/bin/node "$out/bin/pi" \
      --add-flags "$out/lib/node_modules/@earendil-works/pi-coding-agent/dist/cli.js" \
      --prefix PATH : ${pkgs.lib.makeBinPath [ pkgs.ripgrep pkgs.fd ]}
  '';
}
