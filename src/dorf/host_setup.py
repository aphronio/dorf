"""Reviewed host mutations for the first supported Dorf setup recipe."""

from __future__ import annotations

import json
import os
import pwd
import shutil
import stat
from dataclasses import dataclass
from pathlib import Path
from typing import Literal

from dorf.adapters.environments import IncusConfig, IncusRunnerProbe


class HostSetupError(RuntimeError):
    """A reviewed host setup operation failed or found conflicting state."""


GIB = 1024**3
MINIMUM_HOST_MEMORY_BYTES = 4 * GIB
MINIMUM_HOST_DISK_FREE_BYTES = 20 * GIB


@dataclass(frozen=True)
class HostCapacity:
    """Stable Linux facts needed before Dorf attempts a local VM."""

    kvm_available: bool
    cpu_virtualization: bool
    memory_bytes: int
    disk_free_bytes: int


IncusHostRecipe = Literal["arch", "ubuntu-24.04"]


@dataclass(frozen=True)
class IncusHostState:
    """Observed systemd service and user-access facts used for safe convergence."""

    service_enabled: bool
    service_active: bool
    service_restart_required: bool
    admin_membership_configured: bool
    admin_membership_effective: bool

    @property
    def needs_privileged_repair(self) -> bool:
        return (
            not self.service_enabled
            or not self.service_active
            or self.service_restart_required
            or not self.admin_membership_configured
        )


@dataclass(frozen=True)
class IncusVersions:
    """Local Incus client and daemon versions reported by the supported CLI."""

    client: str
    server: str

    @property
    def aligned(self) -> bool:
        return self.client == self.server


def supported_incus_host_recipe(
    *,
    os_release_path: Path = Path("/etc/os-release"),
) -> IncusHostRecipe | None:
    """Select only an exact host recipe with a real clean-host validation terminal."""
    release = _read_os_release(os_release_path)
    os_id = release.get("ID", "")
    if os_id == "arch":
        return "arch"
    if os_id == "ubuntu" and release.get("VERSION_ID") == "24.04":
        return "ubuntu-24.04"
    return None


def host_os_label(*, os_release_path: Path = Path("/etc/os-release")) -> str:
    """Return one bounded distribution label for setup diagnostics and presentation."""
    release = _read_os_release(os_release_path)
    return release.get("PRETTY_NAME") or " ".join(
        value for value in (release.get("ID", ""), release.get("VERSION_ID", "")) if value
    )


def inspect_host_capacity(
    *,
    kvm_path: Path = Path("/dev/kvm"),
    cpuinfo_path: Path = Path("/proc/cpuinfo"),
    meminfo_path: Path = Path("/proc/meminfo"),
    disk_path: Path = Path("/"),
) -> HostCapacity:
    """Inspect bounded Linux virtualization and capacity facts without mutation."""
    try:
        kvm_available = stat.S_ISCHR(kvm_path.stat().st_mode)
    except OSError:
        kvm_available = False
    try:
        cpuinfo = cpuinfo_path.read_text()
    except OSError as error:
        raise HostSetupError(f"Could not read {cpuinfo_path}: {error}") from error
    cpu_virtualization = any(
        flag in {"vmx", "svm"}
        for line in cpuinfo.splitlines()
        if line.startswith(("flags", "Features"))
        for flag in line.partition(":")[2].split()
    )
    try:
        meminfo = meminfo_path.read_text()
        memory_kib = next(
            int(line.split()[1]) for line in meminfo.splitlines() if line.startswith("MemTotal:")
        )
    except (OSError, StopIteration, ValueError) as error:
        raise HostSetupError(f"Could not read total memory from {meminfo_path}") from error
    try:
        disk_free_bytes = shutil.disk_usage(disk_path).free
    except OSError as error:
        raise HostSetupError(f"Could not inspect free disk space at {disk_path}") from error
    return HostCapacity(
        kvm_available=kvm_available,
        cpu_virtualization=cpu_virtualization,
        memory_bytes=memory_kib * 1024,
        disk_free_bytes=disk_free_bytes,
    )


def install_incus_on_arch(
    probe: IncusRunnerProbe,
    *,
    os_release_path: Path = Path("/etc/os-release"),
    username: str | None = None,
) -> None:
    """Install and enable Incus through Arch's official package."""
    if supported_incus_host_recipe(os_release_path=os_release_path) != "arch":
        raise HostSetupError(
            "The Arch Incus recipe does not support "
            f"{host_os_label(os_release_path=os_release_path) or 'this host'}"
        )
    username = username or pwd.getpwuid(os.getuid()).pw_name
    privilege_prefix = _authenticate_administrator(probe)
    _require_command(
        probe,
        [
            *privilege_prefix,
            "pacman",
            "-Syu",
            "--needed",
            "--noconfirm",
            "incus",
        ],
        timeout_seconds=900,
        label="install the Arch Incus package",
    )
    _enable_incus_service_and_access(
        probe,
        privilege_prefix=privilege_prefix,
        username=username,
    )


def install_incus_on_ubuntu_2404(
    probe: IncusRunnerProbe,
    *,
    os_release_path: Path = Path("/etc/os-release"),
    username: str | None = None,
) -> None:
    """Install Ubuntu 24.04's native Incus and QEMU packages."""
    if supported_incus_host_recipe(os_release_path=os_release_path) != "ubuntu-24.04":
        raise HostSetupError(
            "The Ubuntu 24.04 Incus recipe does not support "
            f"{host_os_label(os_release_path=os_release_path) or 'this host'}"
        )
    username = username or pwd.getpwuid(os.getuid()).pw_name
    privilege_prefix = _authenticate_administrator(probe)
    _require_command(
        probe,
        [*privilege_prefix, "apt-get", "update"],
        timeout_seconds=300,
        label="refresh Ubuntu package metadata",
    )
    _require_command(
        probe,
        [
            *privilege_prefix,
            "apt-get",
            "install",
            "--yes",
            "incus",
            "qemu-system",
        ],
        timeout_seconds=900,
        label="install Ubuntu's Incus and QEMU packages",
    )
    _enable_incus_service_and_access(
        probe,
        privilege_prefix=privilege_prefix,
        username=username,
    )


def _enable_incus_service_and_access(
    probe: IncusRunnerProbe,
    *,
    privilege_prefix: list[str],
    username: str,
) -> None:
    """Apply the service and root-equivalent access steps shared by tested systemd hosts."""
    _require_command(
        probe,
        [
            *privilege_prefix,
            "systemctl",
            "enable",
            "--now",
            "incus.service",
        ],
        timeout_seconds=120,
        label="enable the local Incus service",
    )
    _require_command(
        probe,
        [
            *privilege_prefix,
            "usermod",
            "-aG",
            "incus-admin",
            username,
        ],
        timeout_seconds=30,
        label=f"add {username} to incus-admin",
    )


def inspect_incus_host(
    probe: IncusRunnerProbe,
    *,
    os_release_path: Path = Path("/etc/os-release"),
    username: str | None = None,
) -> IncusHostState:
    """Inspect the shared checkpoints on one host with a reviewed Incus recipe."""
    if supported_incus_host_recipe(os_release_path=os_release_path) is None:
        raise HostSetupError(
            "Incus host recovery is not supported for "
            f"{host_os_label(os_release_path=os_release_path) or 'this host'}"
        )
    username = username or pwd.getpwuid(os.getuid()).pw_name
    configured_groups = _read_groups(probe, ["id", "-nG", username])
    effective_groups = _read_groups(probe, ["id", "-nG"])
    service_enabled = _command_succeeds(
        probe,
        ["systemctl", "is-enabled", "--quiet", "incus.service"],
    )
    service_active = _command_succeeds(
        probe,
        ["systemctl", "is-active", "--quiet", "incus.service"],
    )
    try:
        versions = inspect_incus_versions(probe) if service_active else None
    except HostSetupError:
        versions = None
    return IncusHostState(
        service_enabled=service_enabled,
        service_active=service_active,
        service_restart_required=versions is not None and not versions.aligned,
        admin_membership_configured="incus-admin" in configured_groups,
        admin_membership_effective="incus-admin" in effective_groups,
    )


def repair_incus_host(
    probe: IncusRunnerProbe,
    *,
    state: IncusHostState,
    username: str | None = None,
) -> None:
    """Resume only the reviewed systemd service and group changes still missing."""
    username = username or pwd.getpwuid(os.getuid()).pw_name
    commands: list[tuple[list[str], float, str]] = []
    privilege_prefix = [] if os.geteuid() == 0 else ["sudo"]
    if not state.service_enabled or not state.service_active:
        commands.append(
            (
                [
                    *privilege_prefix,
                    "systemctl",
                    "enable",
                    "--now",
                    "incus.service",
                ],
                120,
                "enable the local Incus service",
            )
        )
    elif state.service_restart_required:
        commands.append(
            (
                [*privilege_prefix, "systemctl", "restart", "incus.service"],
                120,
                "restart the local Incus service after its package update",
            )
        )
    if not state.admin_membership_configured:
        commands.append(
            (
                [
                    *privilege_prefix,
                    "usermod",
                    "-aG",
                    "incus-admin",
                    username,
                ],
                30,
                f"add {username} to incus-admin",
            )
        )
    if not commands:
        return
    _authenticate_administrator(probe)
    for argv, timeout_seconds, label in commands:
        _require_command(
            probe,
            argv,
            timeout_seconds=timeout_seconds,
            label=label,
        )


def _authenticate_administrator(probe: IncusRunnerProbe) -> list[str]:
    if os.geteuid() == 0:
        return []
    approval = probe.attach(["sudo", "-v"])
    if approval.returncode != 0:
        raise HostSetupError("Administrator authentication was not granted")
    return ["sudo"]


def inspect_incus_versions(probe: IncusRunnerProbe) -> IncusVersions:
    """Read the local client/daemon pair without depending on package-manager state."""
    result = probe.run(["incus", "version"], timeout_seconds=30)
    if result.returncode != 0:
        raise HostSetupError(f"Could not inspect Incus versions: {_command_detail(result)}")
    values = {
        key.strip().lower(): value.strip()
        for line in result.stdout.splitlines()
        if ":" in line
        for key, value in [line.split(":", 1)]
    }
    client = values.get("client version")
    server = values.get("server version")
    if not client or not server:
        raise HostSetupError("Incus did not report both client and server versions")
    return IncusVersions(client=client, server=server)


def initialize_pristine_incus(
    probe: IncusRunnerProbe,
    *,
    config: IncusConfig,
) -> None:
    """Apply Incus's minimal local-only defaults to a pristine daemon."""
    if config != IncusConfig():
        raise HostSetupError(
            "Automatic initialization supports only the built-in Incus resource names"
        )
    storage = _read_json_list(
        probe,
        ["incus", "storage", "list", "--format", "json"],
        label="Incus storage",
    )
    networks = _read_json_list(
        probe,
        ["incus", "network", "list", "--format", "json"],
        label="Incus networks",
    )
    managed_networks = [
        network
        for network in networks
        if isinstance(network, dict) and network.get("managed") is True
    ]
    if storage or managed_networks:
        raise HostSetupError(
            "Incus is partially initialized; automatic setup will not overwrite it"
        )
    _require_command(
        probe,
        ["incus", "admin", "init", "--minimal"],
        timeout_seconds=120,
        label="initialize local Incus storage and networking",
    )
    if not _read_json_list(
        probe,
        ["incus", "storage", "list", "--format", "json"],
        label="initialized Incus storage",
    ):
        raise HostSetupError("Incus initialization did not create a storage pool")
    network = probe.run(
        ["incus", "network", "show", config.network],
        timeout_seconds=30,
    )
    if network.returncode != 0:
        raise HostSetupError(
            f"Incus initialization did not create private network {config.network}"
        )
    remote_address = probe.run(
        ["incus", "config", "get", "core.https_address"],
        timeout_seconds=30,
    )
    if remote_address.returncode != 0 or remote_address.stdout.strip():
        raise HostSetupError("Incus initialization unexpectedly enabled its remote API")


def _read_os_release(path: Path) -> dict[str, str]:
    try:
        lines = path.read_text().splitlines()
    except OSError as error:
        raise HostSetupError(f"Could not read {path}: {error}") from error
    values: dict[str, str] = {}
    for line in lines:
        if not line or line.startswith("#") or "=" not in line:
            continue
        key, value = line.split("=", 1)
        values[key] = value.strip().strip('"')
    return values


def _read_json_list(
    probe: IncusRunnerProbe,
    argv: list[str],
    *,
    label: str,
) -> list[object]:
    result = probe.run(argv, timeout_seconds=30)
    if result.returncode != 0:
        raise HostSetupError(f"Could not inspect {label}: {_command_detail(result)}")
    try:
        value = json.loads(result.stdout)
    except json.JSONDecodeError as error:
        raise HostSetupError(f"{label} returned invalid JSON") from error
    if not isinstance(value, list):
        raise HostSetupError(f"{label} did not return a list")
    return value


def _read_groups(probe: IncusRunnerProbe, argv: list[str]) -> frozenset[str]:
    result = probe.run(argv, timeout_seconds=30)
    if result.returncode != 0:
        raise HostSetupError(
            f"Could not inspect Incus administrator membership: {_command_detail(result)}"
        )
    return frozenset(result.stdout.split())


def _command_succeeds(probe: IncusRunnerProbe, argv: list[str]) -> bool:
    return probe.run(argv, timeout_seconds=30).returncode == 0


def _require_command(
    probe: IncusRunnerProbe,
    argv: list[str],
    *,
    timeout_seconds: float,
    label: str,
) -> None:
    result = probe.run(argv, timeout_seconds=timeout_seconds)
    if result.returncode != 0:
        raise HostSetupError(f"Could not {label}: {_command_detail(result)}")


def _command_detail(result: object) -> str:
    stderr = getattr(result, "stderr", "")
    stdout = getattr(result, "stdout", "")
    detail = (stderr or stdout).strip()
    if not detail:
        return "command failed without output"
    return detail[-500:]
