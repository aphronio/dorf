let
  pkgs = import ./nixpkgs.nix;
  pins = builtins.fromJSON (builtins.readFile ./packages.json);
  pi = import ./pi.nix { inherit pkgs; };
  browser = import ./browser.nix { inherit pkgs; };
  python = pkgs.python314.withPackages (ps: [ ps.pip ps.setuptools ]);
  uv = pkgs.stdenvNoCC.mkDerivation {
    pname = "uv";
    inherit (pins.uv) version;
    src = pkgs.fetchurl { inherit (pins.uv) url sha256; };
    nativeBuildInputs = [ pkgs.autoPatchelfHook ];
    buildInputs = [ pkgs.stdenv.cc.cc.lib ];
    installPhase = ''mkdir -p "$out/bin"; install -m755 uv "$out/bin/uv"'';
  };
  piLock = builtins.fromJSON (builtins.readFile ./pi/package-lock.json);
  metadata = {
    tools = {
      bash = pkgs.bashInteractive.version; curl = pkgs.curl.version;
      "g++" = pkgs.gcc.version; gcc = pkgs.gcc.version; git = pkgs.git.version;
      go = pkgs.go.version; jq = pkgs.jq.version; make = pkgs.gnumake.version;
      node = pkgs.nodejs_24.version; pip = pkgs.python314Packages.pip.version;
      "pkg-config" = pkgs.pkg-config.version; python = pkgs.python314.version;
      ripgrep = pkgs.ripgrep.version; tar = pkgs.gnutar.version;
      unzip = pkgs.unzip.version; uv = uv.version; wget = pkgs.wget.version;
      "browser-use" = pins.browser.version; "browser-harness" = pins.browser.harness_version;
      "browser-python" = pkgs.python312.version;
      chromium = pins.browser.chromium_version;
    };
    harnesses.pi = {
      package = "@earendil-works/pi-coding-agent"; version = pins.pi.version;
      npm_integrity = piLock.packages."node_modules/@earendil-works/pi-coding-agent".integrity;
      source_url = piLock.packages."node_modules/@earendil-works/pi-coding-agent".resolved;
      package_manager = "nix"; store_path = "${pi}";
    };
    workstation = { nixpkgs_revision = pins.nixpkgs.revision; nixpkgs_hash = pins.nixpkgs.sha256; };
  };
in pkgs.buildEnv {
  name = "dorf-workstation";
  paths = [
    (pkgs.hiPrio python) pkgs.nodejs_24 pkgs.go uv pi browser
    pkgs.bashInteractive pkgs.cacert pkgs.curl pkgs.git pkgs.jq pkgs.gcc
    pkgs.gnumake pkgs.pkg-config pkgs.ripgrep pkgs.gnutar pkgs.unzip pkgs.wget
    pkgs.xz pkgs.coreutils pkgs.findutils pkgs.gnugrep pkgs.gnused pkgs.gzip
  ];
  pathsToLink = [ "/bin" ];
  postBuild = ''
    rm -f "$out/bin/npm" "$out/bin/npx" "$out/bin/corepack"
    mkdir -p "$out/share/dorf"
    cp ${pkgs.writeText "workstation.json" (builtins.toJSON metadata)} "$out/share/dorf/workstation.json"
  '';
}
