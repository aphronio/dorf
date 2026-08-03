"""Reviewed host mutations for the first supported Dorf setup recipe."""

from __future__ import annotations

import json
import os
import pwd
import shutil
import stat
from dataclasses import dataclass
from pathlib import Path

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


@dataclass(frozen=True)
class ArchIncusHostState:
    """Observed Arch service and user-access facts used for safe convergence."""

    service_enabled: bool
    service_active: bool
    admin_membership_configured: bool
    admin_membership_effective: bool

    @property
    def needs_privileged_repair(self) -> bool:
        return (
            not self.service_enabled
            or not self.service_active
            or not self.admin_membership_configured
        )


def host_os_id(*, os_release_path: Path = Path("/etc/os-release")) -> str:
    """Return the stable distribution identifier used to select a reviewed recipe."""
    return _read_os_release(os_release_path).get("ID", "")


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
    detected_os_id = host_os_id(os_release_path=os_release_path)
    if detected_os_id != "arch":
        raise HostSetupError(
            f"Incus installation is not supported for {detected_os_id or 'this host'}"
        )
    username = username or pwd.getpwuid(os.getuid()).pw_name
    privilege_prefix = [] if os.geteuid() == 0 else ["sudo"]
    if privilege_prefix:
        approval = probe.attach(["sudo", "-v"])
        if approval.returncode != 0:
            raise HostSetupError("Administrator authentication was not granted")
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


def inspect_arch_incus_host(
    probe: IncusRunnerProbe,
    *,
    os_release_path: Path = Path("/etc/os-release"),
    username: str | None = None,
) -> ArchIncusHostState:
    """Inspect the exact Arch service and group checkpoints setup owns."""
    detected_os_id = host_os_id(os_release_path=os_release_path)
    if detected_os_id != "arch":
        raise HostSetupError(
            f"Incus host recovery is not supported for {detected_os_id or 'this host'}"
        )
    username = username or pwd.getpwuid(os.getuid()).pw_name
    configured_groups = _read_groups(probe, ["id", "-nG", username])
    effective_groups = _read_groups(probe, ["id", "-nG"])
    return ArchIncusHostState(
        service_enabled=_command_succeeds(
            probe,
            ["systemctl", "is-enabled", "--quiet", "incus.service"],
        ),
        service_active=_command_succeeds(
            probe,
            ["systemctl", "is-active", "--quiet", "incus.service"],
        ),
        admin_membership_configured="incus-admin" in configured_groups,
        admin_membership_effective="incus-admin" in effective_groups,
    )


def repair_arch_incus_host(
    probe: IncusRunnerProbe,
    *,
    state: ArchIncusHostState,
    username: str | None = None,
) -> None:
    """Resume only the reviewed Arch service and group changes still missing."""
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
    if privilege_prefix:
        approval = probe.attach(["sudo", "-v"])
        if approval.returncode != 0:
            raise HostSetupError("Administrator authentication was not granted")
    for argv, timeout_seconds, label in commands:
        _require_command(
            probe,
            argv,
            timeout_seconds=timeout_seconds,
            label=label,
        )


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
