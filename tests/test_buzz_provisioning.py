import os
import subprocess
from pathlib import Path

REPO_ROOT = Path(__file__).parents[1]
NORMALIZE_PUBLIC_KEY = REPO_ROOT / "scripts/incus/normalize-nostr-public-key.py"
PROVISION_HOST = REPO_ROOT / "scripts/incus/provision-buzz.sh"
PROVISION_GUEST = REPO_ROOT / "scripts/incus/provision-buzz-guest.sh"
BUZZ_IMAGE = "ghcr.io/block/buzz:sha-2ce2d71"
PUBLIC_KEY_HEX = "e17e5abf7b1dbd363f0ed6fbda2455609727b2555428dea251388c542cd2f03f"
PUBLIC_KEY_NPUB = "npub1u9l940mmrk7nv0cw6maa5fz4vztj0vj42s5dagj38zx9gtxj7qls94fpux"
OTHER_PUBLIC_KEY_HEX = "a" * 64
RELAY_PRIVATE_KEY_HEX = "b" * 64


def run_normalizer(value: str) -> subprocess.CompletedProcess[str]:
    return subprocess.run(
        [str(NORMALIZE_PUBLIC_KEY), value],
        check=False,
        capture_output=True,
        text=True,
    )


def run_guest_function(
    source_dir: Path,
    command: str,
    *,
    owner_public_key: str = "",
) -> subprocess.CompletedProcess[str]:
    compose_dir = source_dir / "deploy/compose"
    compose_dir.mkdir(parents=True, exist_ok=True)
    environment = {
        **os.environ,
        "BUZZ_SOURCE_DIR": str(source_dir),
        "BUZZ_OWNER_PUBKEY": owner_public_key,
    }
    script = f"""
set -euo pipefail
source "$1"
keypair() {{
  printf '%s\\n' 'Public key:  {"c" * 64}' 'Secret key:  {RELAY_PRIVATE_KEY_HEX}'
}}
secret_hex() {{
  printf '%0*d\\n' "$(( ${{1:-32}} * 2 ))" 0
}}
{command}
"""
    return subprocess.run(
        ["bash", "-c", script, "bash", str(PROVISION_GUEST)],
        check=False,
        capture_output=True,
        env=environment,
        text=True,
    )


def test_public_key_normalizer_accepts_npub_and_hex() -> None:
    npub_result = run_normalizer(PUBLIC_KEY_NPUB)
    uppercase_hex_result = run_normalizer(PUBLIC_KEY_HEX.upper())

    assert npub_result.returncode == 0
    assert npub_result.stdout.strip() == PUBLIC_KEY_HEX
    assert uppercase_hex_result.returncode == 0
    assert uppercase_hex_result.stdout.strip() == PUBLIC_KEY_HEX


def test_public_key_normalizer_rejects_private_or_invalid_input() -> None:
    private_result = run_normalizer("nsec1not-a-public-key")
    bad_checksum_result = run_normalizer(f"{PUBLIC_KEY_NPUB[:-1]}q")

    assert private_result.returncode == 2
    assert "expected an npub public key" in private_result.stderr
    assert bad_checksum_result.returncode == 2
    assert "checksum is invalid" in bad_checksum_result.stderr


def test_new_vm_requires_public_owner_key_before_creation(tmp_path: Path) -> None:
    fake_bin = tmp_path / "bin"
    fake_bin.mkdir()
    mutation_marker = tmp_path / "incus-mutated"
    fake_incus = fake_bin / "incus"
    fake_incus.write_text(
        f"""#!/usr/bin/env bash
if [[ "$1" == "info" ]]; then
  exit 1
fi
touch "{mutation_marker}"
"""
    )
    fake_incus.chmod(0o755)

    result = subprocess.run(
        [str(PROVISION_HOST)],
        check=False,
        capture_output=True,
        env={**os.environ, "PATH": f"{fake_bin}:{os.environ['PATH']}"},
        text=True,
    )

    assert result.returncode == 1
    assert "BUZZ_OWNER_PUBKEY is required" in result.stderr
    assert not mutation_marker.exists()


def test_running_existing_vm_is_not_started_again(tmp_path: Path) -> None:
    fake_bin = tmp_path / "bin"
    fake_bin.mkdir()
    start_marker = tmp_path / "incus-started"
    fake_incus = fake_bin / "incus"
    fake_incus.write_text(
        f"""#!/usr/bin/env bash
case "$1" in
  info)
    exit 0
    ;;
  list)
    printf '%s\\n' RUNNING
    ;;
  start)
    touch "{start_marker}"
    exit 9
    ;;
  exec)
    if [[ "$*" == *"test -f /opt/dorf-buzz/source/deploy/compose/.env"* ]]; then
      exit 0
    fi
    if [[ "$*" == *"/tmp/provision-buzz-guest.sh"* ]]; then
      exit 0
    fi
    exit 0
    ;;
  file)
    exit 0
    ;;
esac
"""
    )
    fake_incus.chmod(0o755)

    result = subprocess.run(
        [str(PROVISION_HOST)],
        check=False,
        capture_output=True,
        env={**os.environ, "PATH": f"{fake_bin}:{os.environ['PATH']}"},
        text=True,
    )

    assert result.returncode == 0, result.stderr
    assert not start_marker.exists()


def test_fresh_guest_environment_uses_only_supplied_owner_public_key(tmp_path: Path) -> None:
    source_dir = tmp_path / "source"

    result = run_guest_function(
        source_dir,
        "write_initial_environment; validate_environment",
        owner_public_key=PUBLIC_KEY_HEX,
    )

    assert result.returncode == 0, result.stderr
    environment = (source_dir / "deploy/compose/.env").read_text()
    assert f"RELAY_OWNER_PUBKEY={PUBLIC_KEY_HEX}\n" in environment
    assert f"BUZZ_RELAY_PRIVATE_KEY={RELAY_PRIVATE_KEY_HEX}\n" in environment
    assert not list(tmp_path.rglob("owner-private-key"))
    assert not list(tmp_path.rglob("owner-public-key"))


def test_fresh_guest_environment_rejects_missing_owner_without_writing(tmp_path: Path) -> None:
    source_dir = tmp_path / "source"

    result = run_guest_function(source_dir, "write_initial_environment")

    assert result.returncode == 1
    assert "normalized BUZZ_OWNER_PUBKEY is required" in result.stderr
    assert not (source_dir / "deploy/compose/.env").exists()


def test_existing_environment_is_retained_and_owner_mismatch_is_rejected(tmp_path: Path) -> None:
    source_dir = tmp_path / "source"
    environment_path = source_dir / "deploy/compose/.env"
    environment_path.parent.mkdir(parents=True)
    original = f"BUZZ_IMAGE={BUZZ_IMAGE}\nRELAY_OWNER_PUBKEY={PUBLIC_KEY_HEX}\n"
    environment_path.write_text(original)

    rerun = run_guest_function(source_dir, "write_initial_environment; validate_environment")
    mismatch = run_guest_function(
        source_dir,
        "write_initial_environment; validate_environment",
        owner_public_key=OTHER_PUBLIC_KEY_HEX,
    )

    assert rerun.returncode == 0, rerun.stderr
    assert mismatch.returncode == 1
    assert "Refusing to replace the configured Buzz owner" in mismatch.stderr
    assert environment_path.read_text() == original
