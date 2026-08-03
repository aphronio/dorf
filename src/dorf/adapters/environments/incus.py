from __future__ import annotations

import ipaddress
import os
import secrets
import shutil
import subprocess
import time
from collections.abc import Callable
from dataclasses import dataclass
from pathlib import Path

DEFAULT_INCUS_TEMPLATE = "dorf-codex"
DEFAULT_INCUS_NETWORK = "incusbr0"
DEFAULT_INCUS_ROOT_DISK_SIZE = "40GiB"
_RFC1918_NETWORKS = tuple(
    ipaddress.ip_network(value) for value in ("10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16")
)


@dataclass(frozen=True)
class IncusConfig:
    template: str = DEFAULT_INCUS_TEMPLATE
    network: str = DEFAULT_INCUS_NETWORK
    root_disk_size: str = DEFAULT_INCUS_ROOT_DISK_SIZE

    @classmethod
    def from_mapping(cls, config: dict[str, str] | None) -> IncusConfig:
        config = config or {}
        return cls(
            template=config.get("template", DEFAULT_INCUS_TEMPLATE),
            network=config.get("network", DEFAULT_INCUS_NETWORK),
            root_disk_size=config.get("root_disk_size", DEFAULT_INCUS_ROOT_DISK_SIZE),
        )


@dataclass(frozen=True)
class IncusFailure:
    code: str
    message: str


@dataclass(frozen=True)
class IncusCheckResult:
    failures: list[IncusFailure]
    remediation: str = ""

    @property
    def ok(self) -> bool:
        return not self.failures


class IncusRunnerProbe:
    def which(self, command: str) -> str | None:
        return shutil.which(command)

    def pull_file(
        self,
        argv: list[str],
        destination: Path,
        *,
        max_bytes: int,
    ) -> subprocess.CompletedProcess[str]:
        try:
            process = subprocess.Popen(
                argv,
                stdout=subprocess.PIPE,
                stderr=subprocess.PIPE,
            )
        except FileNotFoundError:
            return subprocess.CompletedProcess(argv, 127, "", f"{argv[0]} command not found")
        size = 0
        exceeded = False
        assert process.stdout is not None
        with destination.open("xb") as output:
            while chunk := process.stdout.read(1024 * 1024):
                size += len(chunk)
                if size > max_bytes:
                    exceeded = True
                    process.kill()
                    break
                output.write(chunk)
            output.flush()
            os.fsync(output.fileno())
        _, stderr = process.communicate()
        if exceeded:
            destination.unlink(missing_ok=True)
            return subprocess.CompletedProcess(
                argv,
                1,
                "",
                f"file exceeds {max_bytes} byte transfer limit",
            )
        return subprocess.CompletedProcess(
            argv,
            process.returncode,
            "",
            stderr.decode(errors="replace"),
        )

    def attach(self, argv: list[str]) -> subprocess.CompletedProcess[str]:
        """Run one interactive command with the caller's terminal attached."""
        try:
            return subprocess.run(argv, text=True, check=False)
        except FileNotFoundError:
            return subprocess.CompletedProcess(argv, 127, "", f"{argv[0]} command not found")

    def run(
        self,
        argv: list[str],
        *,
        input: str | None = None,
        timeout_seconds: float | None = None,
    ) -> subprocess.CompletedProcess[str]:
        try:
            return subprocess.run(
                argv,
                input=input,
                text=True,
                capture_output=True,
                check=False,
                timeout=timeout_seconds,
            )
        except FileNotFoundError:
            return subprocess.CompletedProcess(argv, 127, "", f"{argv[0]} command not found")
        except subprocess.TimeoutExpired:
            return subprocess.CompletedProcess(
                argv,
                124,
                "",
                f"command timed out after {timeout_seconds} seconds",
            )


def incus_bridge_ipv4(
    network: str,
    *,
    probe: IncusRunnerProbe | None = None,
) -> str:
    """Resolve the RFC1918 host address owned by one selected Incus bridge."""
    probe = probe or IncusRunnerProbe()
    result = probe.run(["incus", "network", "get", network, "ipv4.address"])
    if result.returncode != 0:
        raise RuntimeError(
            command_message(result) or f"Could not resolve Incus network address: {network}"
        )
    try:
        address = ipaddress.ip_interface(result.stdout.strip()).ip
    except ValueError:
        raise RuntimeError(f"Incus network {network} does not have a usable IPv4 address") from None
    if not isinstance(address, ipaddress.IPv4Address) or not any(
        address in private_network for private_network in _RFC1918_NETWORKS
    ):
        raise RuntimeError(f"Incus network {network} does not have a private bridge IPv4 address")
    return str(address)


class IncusDoctor:
    def __init__(
        self,
        probe: IncusRunnerProbe | None = None,
        *,
        sleep: Callable[[float], None] = time.sleep,
    ) -> None:
        self._probe = probe or IncusRunnerProbe()
        self._sleep = sleep

    def fast_check(self, config: IncusConfig) -> IncusCheckResult:
        failures: list[IncusFailure] = []
        if self._probe.which("incus") is None:
            failures.append(IncusFailure("incus-missing", "incus command not found"))
            return IncusCheckResult(
                failures=failures,
                remediation=remediation_commands(
                    failures,
                    network=config.network,
                    egress_interface=detect_egress_interface(self._probe),
                ),
            )

        info = self._probe.run(["incus", "info"])
        if info.returncode != 0:
            failures.append(
                IncusFailure(
                    "incus-access",
                    "current user cannot run incus without sudo",
                )
            )

        network = self._probe.run(["incus", "network", "show", config.network])
        if network.returncode != 0:
            failures.append(
                IncusFailure(
                    "incus-network",
                    f"Incus network not found or inaccessible: {config.network}",
                )
            )

        template = self._probe.run(["incus", "image", "info", config.template])
        if template.returncode != 0:
            failures.append(
                IncusFailure(
                    "incus-template",
                    f"Incus image/template not found or inaccessible: {config.template}",
                )
            )

        return IncusCheckResult(
            failures=failures,
            remediation=remediation_commands(
                failures,
                network=config.network,
                egress_interface=detect_egress_interface(self._probe),
            ),
        )

    def deep_check(
        self,
        config: IncusConfig,
        *,
        probe_name: str | None = None,
    ) -> IncusCheckResult:
        """Run the legacy coding-workstation probe, including Docker tooling."""
        return self._run_probe_check(
            config,
            probe_name=probe_name,
            guest_check=self._guest_failures,
        )

    def core_check(
        self,
        config: IncusConfig,
        *,
        probe_name: str | None = None,
    ) -> IncusCheckResult:
        """Verify only the built-in Room image, guest agent, and private egress."""
        return self._run_probe_check(
            config,
            probe_name=probe_name,
            guest_check=self._network_failures,
        )

    def _run_probe_check(
        self,
        config: IncusConfig,
        *,
        probe_name: str | None,
        guest_check: Callable[[str], list[IncusFailure]],
    ) -> IncusCheckResult:
        fast_result = self.fast_check(config)
        if not fast_result.ok:
            return fast_result

        owns_probe = probe_name is None
        probe_name = probe_name or generate_probe_name()
        failures: list[IncusFailure] = []
        launch = self._probe.run(
            [
                "incus",
                "launch",
                config.template,
                probe_name,
                "--vm",
                "--network",
                config.network,
            ]
        )
        if launch.returncode != 0:
            failures.append(IncusFailure("probe-launch", command_message(launch)))
        elif not self._wait_for_guest_agent(probe_name):
            failures.append(
                IncusFailure(
                    "guest-agent",
                    "Incus guest agent did not become ready for exec",
                )
            )
        else:
            failures.extend(guest_check(probe_name))

        egress_interface = detect_egress_interface(self._probe)
        if owns_probe or launch.returncode == 0:
            self._probe.run(["incus", "delete", probe_name, "--force"])
        return IncusCheckResult(
            failures=failures,
            remediation=remediation_commands(
                failures,
                network=config.network,
                egress_interface=egress_interface,
            ),
        )

    def _wait_for_guest_agent(self, probe_name: str) -> bool:
        for attempt in range(30):
            result = self._probe.run(["incus", "exec", probe_name, "--", "true"])
            if result.returncode == 0:
                return True
            if attempt < 29:
                self._sleep(1)
        return False

    def _guest_failures(self, probe_name: str) -> list[IncusFailure]:
        failures = self._network_failures(probe_name)
        if failures:
            return failures

        install = self._probe.run(
            [
                "incus",
                "exec",
                probe_name,
                "--",
                "bash",
                "-lc",
                (
                    "apt-get update && "
                    "DEBIAN_FRONTEND=noninteractive apt-get install -y "
                    "docker.io docker-compose-v2"
                ),
            ]
        )
        if install.returncode != 0:
            return [
                IncusFailure(
                    "guest-docker-install",
                    command_message(install) or "Docker packages could not be installed",
                )
            ]

        runtime_checks = [
            ("guest-docker", "docker --version && docker info", "Docker is not usable"),
            (
                "guest-docker-compose",
                "docker compose version",
                "Docker Compose is not usable",
            ),
        ]
        for code, command, default_message in runtime_checks:
            result = self._probe.run(["incus", "exec", probe_name, "--", "bash", "-lc", command])
            if result.returncode != 0:
                failures.append(IncusFailure(code, command_message(result) or default_message))
        return failures

    def _network_failures(self, probe_name: str) -> list[IncusFailure]:
        network_checks = [
            (
                "guest-dhcpv4",
                "ip -4 route get 1.1.1.1",
                "guest did not receive DHCPv4/default route",
            ),
            (
                "guest-dns",
                "getent hosts archive.ubuntu.com",
                "guest cannot resolve DNS",
            ),
            (
                "guest-outbound-tcp",
                "timeout 5 bash -lc '</dev/tcp/archive.ubuntu.com/80'",
                "guest cannot open outbound TCP",
            ),
        ]
        failures: list[IncusFailure] = []
        for code, command, default_message in network_checks:
            result = self._probe.run(["incus", "exec", probe_name, "--", "bash", "-lc", command])
            if result.returncode != 0:
                failures.append(IncusFailure(code, command_message(result) or default_message))
        return failures


def command_message(result: subprocess.CompletedProcess[str]) -> str:
    return result.stderr.strip() or result.stdout.strip() or "command failed"


def generate_probe_name() -> str:
    return f"dorf-incus-doctor-{secrets.token_hex(4)}"


def detect_egress_interface(probe: IncusRunnerProbe | None = None) -> str:
    probe = probe or IncusRunnerProbe()
    result = probe.run(["ip", "route", "show", "default"])
    if result.returncode != 0:
        return "<egress-interface>"
    parts = result.stdout.split()
    if "dev" not in parts:
        return "<egress-interface>"
    index = parts.index("dev") + 1
    if index >= len(parts):
        return "<egress-interface>"
    return parts[index]


def remediation_commands(
    failures: list[IncusFailure],
    *,
    network: str,
    egress_interface: str,
) -> str:
    codes = {failure.code for failure in failures}
    commands: list[str] = []
    if "incus-missing" in codes:
        commands.extend(
            [
                "sudo pacman -S --needed incus dnsmasq lxc lxcfs",
                "sudo systemctl enable --now incus",
            ]
        )
    if "incus-access" in codes:
        commands.extend(
            [
                'sudo usermod -aG incus-admin "$USER"',
                "newgrp incus-admin",
                "incus admin init --minimal",
            ]
        )
    if "guest-dhcpv4" in codes:
        commands.append(f"sudo ufw allow in on {network} proto udp from any port 68 to any port 67")
    if codes.intersection({"guest-dns", "guest-outbound-tcp"}):
        commands.append(f"sudo ufw route allow in on {network} out on {egress_interface}")
    return "\n".join(dict.fromkeys(commands))
