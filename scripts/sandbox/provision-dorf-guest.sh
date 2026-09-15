#!/usr/bin/env bash
set -euo pipefail
export DEBIAN_FRONTEND=noninteractive
readonly PACKAGE_DIR=/usr/local/share/dorf/packages
readonly TOOL_PROFILE=/nix/var/nix/profiles/dorf-tools

source /etc/os-release
if [[ "${ID:-}" != debian || "${VERSION_ID:-}" != 13 ]]; then
  echo "Dorf's supported Sandbox profile requires Debian 13." >&2
  exit 1
fi
if [[ -z "${DORF_BASE_IMAGE:-}" || ! "${DORF_BASE_FINGERPRINT:-}" =~ ^[0-9a-f]{64}$ ]]; then
  echo "An exact Debian base reference and identity are required." >&2
  exit 1
fi

# Only the provider OS and the tools required to bootstrap Nix come from Debian.
apt-get update
apt-get install -y --no-install-recommends bash ca-certificates curl python3 tar xz-utils
chmod 0755 "$PACKAGE_DIR/guest.sh"
ln -sfn "$PACKAGE_DIR/guest.sh" /usr/local/bin/dorf-packages
dorf-packages bootstrap
for tool in nix nix-build nix-env nix-store; do
  ln -sfn "/root/.nix-profile/bin/$tool" "/usr/local/bin/$tool"
done
dorf-packages install-workstation
for executable in "$TOOL_PROFILE"/bin/*; do
  [[ -x "$executable" ]] || continue
  ln -sfn "$executable" "/usr/local/bin/$(basename "$executable")"
done
hash -r

CODEX_VERSION="$(dorf-packages default-version)"
dorf-packages stage "$CODEX_VERSION"
dorf-packages activate "$CODEX_VERSION"
pi --version
browser-use skill install --target codex --no-install
browser-use recordings enable

install -d -m 0755 /usr/local/share/dorf /workspace/job
jq \
  --arg base_reference "$DORF_BASE_IMAGE" \
  --arg base_fingerprint "$DORF_BASE_FINGERPRINT" \
  --arg codex_version "$CODEX_VERSION" \
  --arg codex_integrity "$(jq -r --arg v "$CODEX_VERSION" '.codex[$v].hash' "$PACKAGE_DIR/packages.json")" \
  --arg codex_source "$(jq -r --arg v "$CODEX_VERSION" '.codex[$v].url' "$PACKAGE_DIR/packages.json")" \
  --arg codex_path "$(readlink -f /nix/var/nix/profiles/dorf-runner)" \
  --arg workstation_path "$(readlink -f "$TOOL_PROFILE")" \
  --arg nix_version "$(nix --version | awk '{print $3}')" \
  --arg nix_integrity "sha256:$(jq -r .nix.sha256 "$PACKAGE_DIR/packages.json")" \
  '.base_image = {reference: $base_reference, fingerprint: $base_fingerprint}
   | .harnesses.codex = {package: "@openai/codex", version: $codex_version,
       npm_integrity: $codex_integrity, source_url: $codex_source,
       package_manager: "nix", store_path: $codex_path}
   | .workstation.store_path = $workstation_path
   | .tools.nix = $nix_version
   | .tool_integrity = {nix: $nix_integrity}' \
  "$TOOL_PROFILE/share/dorf/workstation.json" > /usr/local/share/dorf/image.json
chmod 0644 /usr/local/share/dorf/image.json

# Retain installed profiles and staged generations, discard construction-only dependencies.
NIX_CONFIG='build-users-group =' nix-store --gc
apt-get clean
rm -rf -- /var/lib/apt/lists/* /root/.cache
rm -f /root/.bash_history
truncate -s 0 /etc/machine-id || true
rm -f /var/lib/dbus/machine-id
