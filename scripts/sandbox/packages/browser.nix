{ pkgs ? import ./nixpkgs.nix }:
let
  pins = (builtins.fromJSON (builtins.readFile ./packages.json)).browser;
  # Reuse Chromium binary fixups only; this does not install the Playwright driver.
  chromium = pkgs.playwright-driver.components.chromium.overrideAttrs (old: {
    src = pkgs.fetchurl pins.chromium;
    nativeBuildInputs = old.nativeBuildInputs ++ [ pkgs.unzip ];
  });
  wheels = builtins.fromJSON (builtins.readFile ./browser-wheels.json);
  wheelhouse = pkgs.linkFarm "dorf-browser-wheels" (map (wheel: {
    name = builtins.baseNameOf wheel.url;
    path = pkgs.fetchurl wheel;
  }) wheels);
  chromiumBin = "${chromium}/chrome-linux64/chrome";
in pkgs.stdenvNoCC.mkDerivation {
  pname = "dorf-browser-tools";
  inherit (pins) version;
  dontUnpack = true;
  nativeBuildInputs = [ pkgs.uv pkgs.autoPatchelfHook pkgs.makeWrapper ];
  buildInputs = [ pkgs.stdenv.cc.cc.lib pkgs.zlib ];
  installPhase = ''
    export UV_CACHE_DIR="$TMPDIR/uv-cache"
    uv venv --python ${pkgs.python312}/bin/python3 "$out"
    uv pip install --no-cache --no-index --find-links ${wheelhouse} --python "$out/bin/python" \
      browser-use==${pins.version} browser-harness==${pins.harness_version}
    makeWrapper ${chromiumBin} "$out/bin/chromium" \
      --set-default SSL_CERT_FILE ${pkgs.cacert}/etc/ssl/certs/ca-bundle.crt
    ln -s chromium "$out/bin/dorf-chromium"
    ln -s python "$out/bin/browser-python"
    wrapProgram "$out/bin/browser-use" --set-default BH_CHROME_PATH ${chromiumBin}
  '';
  passthru = { inherit chromium; };
}
