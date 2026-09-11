#!/usr/bin/env bash
set -euo pipefail

export DEBIAN_FRONTEND=noninteractive
export UV_PYTHON_INSTALL_DIR=/opt/uv-python
export UV_TOOL_DIR=/opt/browser-use/tools
export UV_TOOL_BIN_DIR=/opt/browser-use/bin
export PLAYWRIGHT_BROWSERS_PATH=/opt/browser-use/browsers

readonly BROWSER_USE_VERSION=0.13.10
readonly BROWSER_HARNESS_VERSION=0.1.13
readonly PLAYWRIGHT_VERSION=1.62.0
readonly BROWSER_PYTHON_VERSION=3.12.13

uv tool install --python "$BROWSER_PYTHON_VERSION" \
  --with "browser-harness==$BROWSER_HARNESS_VERSION" "browser-use==$BROWSER_USE_VERSION"
uv tool install --python "$BROWSER_PYTHON_VERSION" "playwright==$PLAYWRIGHT_VERSION"
/opt/browser-use/bin/playwright install chromium --with-deps --no-shell
/opt/browser-use/bin/browser-use skill install --target codex --no-install
/opt/browser-use/bin/browser-use recordings enable

browser_executable="$(/opt/browser-use/tools/playwright/bin/python -c \
  'from playwright.sync_api import sync_playwright
with sync_playwright() as p:
    print(p.chromium.executable_path)')"
ln -sf "$browser_executable" /usr/local/bin/dorf-chromium

cat >/etc/systemd/system/browser-use-vm-browser.service <<'UNIT'
[Unit]
Description=Isolated browser for Dorf Sandbox agents
After=network.target

[Service]
Type=simple
ExecStartPre=/usr/bin/mkdir -p /root/.local/share/browser-use-vm/chromium-profile
ExecStart=/usr/local/bin/dorf-chromium --headless=new --no-sandbox --disable-dev-shm-usage --remote-debugging-address=127.0.0.1 --remote-debugging-port=9222 --user-data-dir=/root/.local/share/browser-use-vm/chromium-profile --no-first-run --no-default-browser-check about:blank
Restart=on-failure
RestartSec=1

[Install]
WantedBy=multi-user.target
UNIT
systemctl enable browser-use-vm-browser.service

cat >/usr/local/bin/browser-use <<'WRAPPER'
#!/usr/bin/env bash
set -euo pipefail
export BU_CDP_URL=http://127.0.0.1:9222
export BH_TAB_MARKER=0
if ! curl --fail --silent --max-time 2 "$BU_CDP_URL/json/version" >/dev/null; then
  systemctl start browser-use-vm-browser.service
  ready=false
  for _ in {1..100}; do
    if curl --fail --silent --max-time 2 "$BU_CDP_URL/json/version" >/dev/null; then
      ready=true
      break
    fi
    systemctl is-active --quiet browser-use-vm-browser.service
    sleep 0.1
  done
  if [[ "$ready" != true ]]; then
    echo 'Browser did not become ready; inspect journalctl -u browser-use-vm-browser.' >&2
    exit 1
  fi
fi
exec /opt/browser-use/bin/browser-use "$@"
WRAPPER
chmod 0755 /usr/local/bin/browser-use

metadata=/usr/local/share/dorf/image.json
jq --arg browser_use "$BROWSER_USE_VERSION" \
  --arg browser_harness "$BROWSER_HARNESS_VERSION" \
  --arg playwright "$PLAYWRIGHT_VERSION" \
  --arg browser_python "$BROWSER_PYTHON_VERSION" \
  --arg chromium "$("$browser_executable" --version)" \
  '.tools += {"browser-use": $browser_use, "browser-harness": $browser_harness,
    playwright: $playwright, "browser-python": $browser_python, chromium: $chromium}' \
  "$metadata" >"$metadata.tmp"
mv "$metadata.tmp" "$metadata"

apt-get clean
rm -rf -- /var/lib/apt/lists/* /root/.cache
