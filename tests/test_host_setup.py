import json
import subprocess
from types import SimpleNamespace

import pytest

from dorf.adapters.environments import IncusConfig
from dorf.host_setup import (
    GIB,
    HostSetupError,
    IncusHostState,
    initialize_pristine_incus,
    inspect_host_capacity,
    inspect_incus_host,
    install_incus_on_arch,
    install_incus_on_ubuntu_2404,
    repair_incus_host,
    supported_incus_host_recipe,
)


class FakeHostProbe:
    def __init__(
        self,
        *,
        initialized: bool = False,
        partial: bool = False,
        service_enabled: bool = True,
        service_active: bool = True,
        client_version: str = "7.2",
        server_version: str = "7.2",
        configured_groups: str = "dorf-test-user incus-admin",
        effective_groups: str = "dorf-test-user incus-admin",
    ) -> None:
        self.initialized = initialized
        self.partial = partial
        self.service_enabled = service_enabled
        self.service_active = service_active
        self.client_version = client_version
        self.server_version = server_version
        self.configured_groups = configured_groups
        self.effective_groups = effective_groups
        self.attached: list[list[str]] = []
        self.ran: list[list[str]] = []

    def attach(self, argv):
        self.attached.append(argv)
        return subprocess.CompletedProcess(argv, 0, "", "")

    def run(self, argv, *, input=None, timeout_seconds=None):
        self.ran.append(argv)
        if argv == ["systemctl", "is-enabled", "--quiet", "incus.service"]:
            return subprocess.CompletedProcess(argv, 0 if self.service_enabled else 1, "", "")
        if argv == ["systemctl", "is-active", "--quiet", "incus.service"]:
            return subprocess.CompletedProcess(argv, 0 if self.service_active else 1, "", "")
        if argv == ["id", "-nG", "dorf-test-user"]:
            return subprocess.CompletedProcess(argv, 0, self.configured_groups, "")
        if argv == ["id", "-nG"]:
            return subprocess.CompletedProcess(argv, 0, self.effective_groups, "")
        if argv == ["incus", "version"]:
            return subprocess.CompletedProcess(
                argv,
                0,
                (
                    f"Client version: {self.client_version}\n"
                    f"Server version: {self.server_version}\n"
                ),
                "",
            )
        if argv == ["incus", "storage", "list", "--format", "json"]:
            storage = (
                [{"name": "default", "driver": "dir", "status": "CREATED"}]
                if self.initialized or self.partial
                else []
            )
            return subprocess.CompletedProcess(argv, 0, json.dumps(storage), "")
        if argv == ["incus", "network", "list", "--format", "json"]:
            networks = (
                [{"name": "incusbr0", "managed": True}]
                if self.initialized
                else [{"name": "eth0", "managed": False}]
            )
            return subprocess.CompletedProcess(argv, 0, json.dumps(networks), "")
        if argv == ["incus", "admin", "init", "--minimal"]:
            self.initialized = True
        if argv == ["incus", "network", "show", "incusbr0"] and not self.initialized:
            return subprocess.CompletedProcess(argv, 1, "", "not found")
        if argv == ["incus", "config", "get", "core.https_address"]:
            return subprocess.CompletedProcess(argv, 0, "\n", "")
        return subprocess.CompletedProcess(argv, 0, "", "")


def test_host_capacity_reads_stable_linux_virtualization_memory_and_disk_facts(
    tmp_path,
    monkeypatch,
) -> None:
    kvm = tmp_path / "kvm"
    cpuinfo = tmp_path / "cpuinfo"
    meminfo = tmp_path / "meminfo"
    kvm.write_text("")
    cpuinfo.write_text("flags : fpu vmx sse\n")
    meminfo.write_text(f"MemTotal: {8 * GIB // 1024} kB\n")
    monkeypatch.setattr("dorf.host_setup.stat.S_ISCHR", lambda mode: True)
    monkeypatch.setattr(
        "dorf.host_setup.shutil.disk_usage",
        lambda path: SimpleNamespace(free=50 * GIB),
    )

    capacity = inspect_host_capacity(
        kvm_path=kvm,
        cpuinfo_path=cpuinfo,
        meminfo_path=meminfo,
        disk_path=tmp_path,
    )

    assert capacity.kvm_available is True
    assert capacity.cpu_virtualization is True
    assert capacity.memory_bytes == 8 * GIB
    assert capacity.disk_free_bytes == 50 * GIB


def test_arch_recipe_uses_official_package_service_and_admin_group(
    tmp_path,
    monkeypatch,
) -> None:
    os_release = tmp_path / "os-release"
    os_release.write_text('ID=arch\nPRETTY_NAME="Arch Linux"\n')
    probe = FakeHostProbe()
    monkeypatch.setattr("dorf.host_setup.os.geteuid", lambda: 1000)

    install_incus_on_arch(
        probe,
        os_release_path=os_release,
        username="dorf-test-user",
    )

    assert probe.attached == [["sudo", "-v"]]
    assert probe.ran == [
        [
            "sudo",
            "pacman",
            "-Syu",
            "--needed",
            "--noconfirm",
            "incus",
        ],
        [
            "sudo",
            "systemctl",
            "enable",
            "--now",
            "incus.service",
        ],
        ["sudo", "usermod", "-aG", "incus-admin", "dorf-test-user"],
    ]


def test_ubuntu_2404_recipe_uses_native_packages_service_and_admin_group(
    tmp_path,
    monkeypatch,
) -> None:
    os_release = tmp_path / "os-release"
    os_release.write_text(
        'ID=ubuntu\nVERSION_ID="24.04"\nPRETTY_NAME="Ubuntu 24.04.3 LTS"\n'
    )
    probe = FakeHostProbe()
    monkeypatch.setattr("dorf.host_setup.os.geteuid", lambda: 1000)

    install_incus_on_ubuntu_2404(
        probe,
        os_release_path=os_release,
        username="dorf-test-user",
    )

    assert probe.attached == [["sudo", "-v"]]
    assert probe.ran == [
        ["sudo", "apt-get", "update"],
        [
            "sudo",
            "apt-get",
            "install",
            "--yes",
            "incus",
            "qemu-system",
        ],
        [
            "sudo",
            "systemctl",
            "enable",
            "--now",
            "incus.service",
        ],
        ["sudo", "usermod", "-aG", "incus-admin", "dorf-test-user"],
    ]


@pytest.mark.parametrize(
    ("os_release", "expected"),
    [
        ("ID=arch\n", "arch"),
        ('ID=ubuntu\nVERSION_ID="24.04"\n', "ubuntu-24.04"),
        ('ID=ubuntu\nVERSION_ID="26.04"\n', None),
        ('ID=debian\nVERSION_ID="13"\n', None),
    ],
)
def test_recipe_selection_claims_only_clean_host_validated_distributions(
    tmp_path,
    os_release,
    expected,
) -> None:
    path = tmp_path / "os-release"
    path.write_text(os_release)

    assert supported_incus_host_recipe(os_release_path=path) == expected


def test_minimal_initialization_requires_pristine_state_and_keeps_remote_api_off() -> None:
    probe = FakeHostProbe()

    initialize_pristine_incus(probe, config=IncusConfig())

    assert ["incus", "admin", "init", "--minimal"] in probe.ran
    assert ["incus", "network", "show", "incusbr0"] in probe.ran
    assert ["incus", "config", "get", "core.https_address"] in probe.ran


def test_minimal_initialization_refuses_partially_owned_incus() -> None:
    probe = FakeHostProbe(partial=True)

    with pytest.raises(HostSetupError, match="partially initialized"):
        initialize_pristine_incus(probe, config=IncusConfig())

    assert ["incus", "admin", "init", "--minimal"] not in probe.ran


@pytest.mark.parametrize(
    "release",
    ["ID=arch\n", 'ID=ubuntu\nVERSION_ID="24.04"\n'],
)
def test_reviewed_host_recovery_inspects_and_applies_only_missing_checkpoints(
    tmp_path,
    monkeypatch,
    release,
) -> None:
    os_release = tmp_path / "os-release"
    os_release.write_text(release)
    probe = FakeHostProbe(
        service_enabled=False,
        service_active=False,
        configured_groups="dorf-test-user wheel",
        effective_groups="dorf-test-user wheel",
    )
    monkeypatch.setattr("dorf.host_setup.os.geteuid", lambda: 1000)

    state = inspect_incus_host(
        probe,
        os_release_path=os_release,
        username="dorf-test-user",
    )
    repair_incus_host(probe, state=state, username="dorf-test-user")

    assert state == IncusHostState(
        service_enabled=False,
        service_active=False,
        service_restart_required=False,
        admin_membership_configured=False,
        admin_membership_effective=False,
    )
    assert probe.attached == [["sudo", "-v"]]
    assert [
        "sudo",
        "systemctl",
        "enable",
        "--now",
        "incus.service",
    ] in probe.ran
    assert [
        "sudo",
        "usermod",
        "-aG",
        "incus-admin",
        "dorf-test-user",
    ] in probe.ran


def test_reviewed_host_recovery_makes_no_changes_when_checkpoints_are_complete(
    tmp_path,
) -> None:
    os_release = tmp_path / "os-release"
    os_release.write_text("ID=arch\n")
    probe = FakeHostProbe()

    state = inspect_incus_host(
        probe,
        os_release_path=os_release,
        username="dorf-test-user",
    )
    repair_incus_host(probe, state=state, username="dorf-test-user")

    assert not probe.attached
    assert not any(command[0] == "sudo" for command in probe.ran)


def test_reviewed_host_recovery_restarts_a_stale_daemon_after_package_update(
    tmp_path,
    monkeypatch,
) -> None:
    os_release = tmp_path / "os-release"
    os_release.write_text('ID=ubuntu\nVERSION_ID="24.04"\n')
    probe = FakeHostProbe(client_version="7.2", server_version="7.0")
    monkeypatch.setattr("dorf.host_setup.os.geteuid", lambda: 1000)

    state = inspect_incus_host(
        probe,
        os_release_path=os_release,
        username="dorf-test-user",
    )
    repair_incus_host(probe, state=state, username="dorf-test-user")

    assert state.service_restart_required is True
    assert ["sudo", "systemctl", "restart", "incus.service"] in probe.ran
