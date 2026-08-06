"""Integrity-pinned Node and Pi tooling for disposable verifier Rooms.

This first slice installs the pinned toolchain inside each disposable verifier
Room instead of expanding the official image. Everything here is credential-free:
upstream API keys never appear in these plans, scripts, or the Pi extension; the
Room's broker-local route credential is read from the existing route-key file
installed by the Room composition.
"""

from __future__ import annotations

import textwrap
from dataclasses import dataclass

from dorf.provider_gateway import InferenceRoute

# Pi 0.83.0 declares engines.node ">=22.19.0"; 22.23.2 is the latest official
# v22 LTS release (2026-07-28) satisfying that engine.
NODE_VERSION = "22.23.2"
NODE_TARBALL = f"node-v{NODE_VERSION}-linux-x64.tar.gz"
NODE_URL = f"https://nodejs.org/dist/v{NODE_VERSION}/{NODE_TARBALL}"
# Official nodejs.org SHASUMS256.txt digest for the exact pinned tarball,
# verified against https://nodejs.org/dist/v22.23.2/SHASUMS256.txt. Use gzip
# because the current credential-free official image intentionally omits xz.
NODE_SHA256 = "b294a556e639d64338823920e5866c21c02741742d2e1529ee1a225c1ec9252a"

PI_VERSION = "0.83.0"
PI_TARBALL = f"pi-coding-agent-{PI_VERSION}.tgz"
PI_URL = f"https://registry.npmjs.org/@earendil-works/pi-coding-agent/-/{PI_TARBALL}"
# npm registry dist.integrity (sha512) for the exact pinned package tarball.
# npm publishes this payload base64-encoded; the install script therefore
# compares the tarball digest in base64 (not openssl hex) form.
PI_SHA512 = (
    "uYhF+FsZxogoSX/AxBcUdiY+ZklubwaXyAoEGA2eQwsHcyEAhUYIKh/WLXe/"
    "a8+k8eTCmxb+ZN2Zo9mzQtzbWw=="
)


PI_INSTALL_ROOT = "/opt/dorf/verifier"
PI_NODE_DIR = f"{PI_INSTALL_ROOT}/node-v{NODE_VERSION}"
# Isolated global prefix for the Pi package: pinned npm's global install writes
# the launcher at $prefix/bin/pi and the package at
# $prefix/lib/node_modules/@earendil-works/pi-coding-agent.
PI_PREFIX = PI_INSTALL_ROOT
PI_PACKAGE_DIR = f"{PI_PREFIX}/lib/node_modules/@earendil-works/pi-coding-agent"
PI_CLI_PATH = f"{PI_PACKAGE_DIR}/dist/cli.js"
PI_BIN_PATH = f"{PI_PREFIX}/bin/pi"
# The route extension lives at a stable private path outside the npm package so
# an idempotent reinstall never overwrites it or the package metadata.
PI_EXTENSION_PATH = f"{PI_INSTALL_ROOT}/extensions/dorf-deepseek-provider.mjs"
PI_PROTOCOL_PATH = "/workspace/review-protocol.txt"
PI_SESSION_DIR = "/tmp/dorf/pi-sessions"

# Pi built-in tool names; the review role gets read/search tools only.
PI_READ_ONLY_TOOLS = ("read", "grep", "find", "ls")
PI_PROVIDER_NAME = "dorf-deepseek"

# Pi thinking levels (docs/models.md); Codex's `ultra` is not a Pi level.
PI_REASONING_EFFORTS = frozenset({"minimal", "low", "medium", "high", "xhigh", "max"})


@dataclass(frozen=True)
class ToolInstallPlan:
    """One pinned, digest-verified tool artifact for the verifier Room.

    ``install_directory`` is the extraction root for archive artifacts (Node)
    and the isolated global prefix for npm-installed artifacts (Pi).
    """

    label: str
    url: str
    filename: str
    digest_algorithm: str
    expected_digest: str
    install_directory: str


def node_install_plan() -> ToolInstallPlan:
    return ToolInstallPlan(
        label="Node.js",
        url=NODE_URL,
        filename=NODE_TARBALL,
        digest_algorithm="sha256",
        expected_digest=NODE_SHA256,
        install_directory=PI_NODE_DIR,
    )


def pi_install_plan() -> ToolInstallPlan:
    return ToolInstallPlan(
        label="Pi coding agent",
        url=PI_URL,
        filename=PI_TARBALL,
        digest_algorithm="sha512",
        expected_digest=PI_SHA512,
        install_directory=PI_PREFIX,
    )


def install_verifier_tooling_script(
    *,
    node_plan: ToolInstallPlan | None = None,
    pi_plan: ToolInstallPlan | None = None,
    install_root: str | None = None,
) -> str:
    """One deterministic, idempotent, credential-free toolchain install script.

    Node is extracted from its verified official archive (one top-level release
    directory, so extraction strips exactly one component). Pi is installed
    directly from its verified local tarball through the pinned Node/npm into an
    isolated global prefix: the published package is never unpacked and npm
    never runs from inside it. npm resolves the shrinkwrap and writes the
    launcher at ``$prefix/bin/pi`` with the package at
    ``$prefix/lib/node_modules/@earendil-works/pi-coding-agent``.
    """
    node = node_plan or node_install_plan()
    pi = pi_plan or pi_install_plan()
    node_dir = node.install_directory
    node_bin = f"{node_dir}/bin"
    pi_prefix = pi.install_directory
    root = install_root or PI_INSTALL_ROOT
    return textwrap.dedent(
        f"""
        set -euo pipefail
        install_root={root}
        mkdir -p "$install_root"
        cd "$install_root"

        install_tool() {{
          label="$1" url="$2" filename="$3" algorithm="$4" expected="$5" target_dir="$6"
          if test -f "$target_dir/.installed"; then
            echo "verifier-tooling: $label already installed"
            return 0
          fi
          staging="$install_root/.$filename.$$"
          trap 'rm -f "$staging"' EXIT
          curl --fail --silent --show-error --location --max-time 300 \\
            "$url" --output "$staging"
          if test "$algorithm" = "sha512"; then
            # npm registry integrity is base64; openssl -r emits hex.
            actual=$(openssl dgst -sha512 -binary "$staging" | base64 -w0)
          else
            actual=$(openssl dgst -"$algorithm" -r "$staging" | cut -d' ' -f1)
          fi
          if test "$actual" != "$expected"; then
            echo "verifier-tooling: $label digest mismatch" >&2
            exit 1
          fi
          tmp_dir="$install_root/.$filename.dir.$$"
          mkdir -p "$tmp_dir"
          tar --strip-components=1 -xzf "$staging" -C "$tmp_dir"
          rm -f "$staging"
          rm -rf "$target_dir"
          mv "$tmp_dir" "$target_dir"
          touch "$target_dir/.installed"
          trap - EXIT
          echo "verifier-tooling: installed $label"
        }}
        install_tool Node.js {node.url} {node.filename} {node.digest_algorithm} \\
          {node.expected_digest} {node_dir}

        # Pi: download and SHA-512 (base64, npm integrity encoding) verify the
        # published tarball, then install it directly from that verified local
        # tarball through the pinned Node/npm into the isolated global prefix.
        # The package is never unpacked and npm never runs from inside it; npm
        # resolves the shrinkwrap and writes $prefix/bin/pi plus the package
        # under $prefix/lib/node_modules/@earendil-works/pi-coding-agent.
        pi_prefix={pi_prefix}
        pi_bin="$pi_prefix/bin/pi"
        pi_package_dir="$pi_prefix/lib/node_modules/@earendil-works/pi-coding-agent"
        if test -x "$pi_bin" && test -f "$pi_package_dir/dist/cli.js"; then
          echo "verifier-tooling: Pi coding agent already installed"
        else
          pi_staging="$install_root/.$$.{pi.filename}"
          pi_cleanup() {{
            rm -f "$pi_staging"
            if test "${{pi_ok:-0}}" != 1; then
              rm -rf "$pi_package_dir"
              rm -f "$pi_bin"
            fi
          }}
          trap pi_cleanup EXIT
          curl --fail --silent --show-error --location --max-time 300 \\
            "{pi.url}" --output "$pi_staging"
          actual=$(openssl dgst -sha512 -binary "$pi_staging" | base64 -w0)
          if test "$actual" != "{pi.expected_digest}"; then
            echo "verifier-tooling: Pi coding agent digest mismatch" >&2
            exit 1
          fi
          PATH="{node_bin}:$PATH" "{node_bin}/npm" install --global \\
            --prefix "$pi_prefix" "$pi_staging" --omit=dev --no-audit --no-fund \
            --cache "$install_root/.npm-cache"
          pi_ok=1
          rm -f "$pi_staging"
          trap - EXIT
          echo "verifier-tooling: installed Pi coding agent"
        fi
        echo "verifier-tooling: ready"
        """
    ).strip()


def pi_provider_extension(
    *,
    route: InferenceRoute | None = None,
    model: str = "deepseek-v4-flash",
    prefix: str = "deepseek",
) -> str:
    """The Pi extension that pins the DeepSeek scoped route for one Room.

    The model id is the prefixed broker model (``prefix/model``): prefix pinning
    guarantees this Pi invocation and role can only reference the intended
    DeepSeek Pi model id, and the session exposes only the configured read-only
    tools. The API key is the Room's broker-local route credential read from the
    environment at runtime; CLIProxyAPI keys are broker keys, not cryptographic
    per-provider allowlists, so the route credential itself is not claimed to be
    provider-exclusive. Exact live upstream affinity is only established by the
    real broker terminal.
    """
    base_url = route.base_url if route is not None else "http://127.0.0.1:8317/v1"
    prefixed_model = f"{prefix}/{model}"
    return textwrap.dedent(
        f"""
        export default function (pi) {{
          pi.registerProvider({PI_PROVIDER_NAME!r}, {{
            baseUrl: {base_url!r},
            apiKey: "${{DORF_PROVIDER_ROUTE_KEY}}",
            api: "openai-responses",
            models: [{{
              id: {prefixed_model!r},
              name: "DeepSeek V4 Flash",
              reasoning: true,
              input: ["text"],
              cost: {{ input: 0, output: 0, cacheRead: 0, cacheWrite: 0 }},
              contextWindow: 1000000,
              maxTokens: 384000,
              thinkingLevelMap: {{
                minimal: null,
                low: null,
                medium: null,
                high: "default",
                xhigh: null,
                max: "max"
              }},
              compat: {{
                supportsDeveloperRole: false,
                supportsReasoningEffort: true,
                maxTokensField: "max_tokens"
              }}
            }}]
          }});
        }}
        """
    ).strip()


def pi_command(
    *,
    role_name: str,
    model: str,
    prefix: str,
    reasoning_effort: str,
    run_id: int,
) -> list[str]:
    """The one pinned Pi review invocation for a fresh read-only session."""
    node_bin = f"{PI_NODE_DIR}/bin"
    prefixed_model = f"{prefix}/{model}"
    return [
        "bash",
        "-lc",
        (
            f"export PATH={node_bin}:$PATH; "
            f"exec {node_bin}/node {PI_CLI_PATH} -p "
            f"--provider {PI_PROVIDER_NAME} "
            f"--model {PI_PROVIDER_NAME}/{prefixed_model} "
            f"--thinking {reasoning_effort} "
            f"--tools {','.join(PI_READ_ONLY_TOOLS)} "
            "--no-session --no-approve --no-context-files "
            "--no-extensions "
            f"-e {PI_EXTENSION_PATH} "
            f"--session-dir {PI_SESSION_DIR}/{run_id} "
            f"--name dorf-verifier-{role_name} "
            f"@{PI_PROTOCOL_PATH}"
        ),
    ]


__all__ = [
    "NODE_SHA256",
    "NODE_URL",
    "NODE_VERSION",
    "PI_BIN_PATH",
    "PI_CLI_PATH",
    "PI_EXTENSION_PATH",
    "PI_PACKAGE_DIR",
    "PI_PREFIX",
    "PI_PROTOCOL_PATH",
    "PI_READ_ONLY_TOOLS",
    "PI_REASONING_EFFORTS",
    "PI_SHA512",
    "PI_URL",
    "PI_VERSION",
    "ToolInstallPlan",
    "install_verifier_tooling_script",
    "node_install_plan",
    "pi_command",
    "pi_install_plan",
    "pi_provider_extension",
]
