import json
import subprocess
from contextlib import nullcontext
from pathlib import Path
from types import SimpleNamespace

import pytest
from typer.testing import CliRunner

from dorf.cli import _ImageDownloadProgress, app
from dorf.core_setup import (
    EXPECTED_SETUP_RESPONSE,
    CoreSetup,
    CoreSetupFailed,
    CoreSetupPaused,
    CoreSetupResult,
)
from dorf.deployment_profile import load_deployment_profile
from dorf.host_setup import GIB, HostCapacity
from dorf.official_image import (
    OfficialImageError,
    OfficialImageInstallResult,
)
from dorf.provider_gateway import DeviceAuthorization, ProviderConnection

FINGERPRINT = "a" * 64
USE_READY_IMAGE = object()


class FakeProbe:
    def __init__(
        self,
        *,
        installed: bool = True,
        incus_access: bool = True,
        image_present: bool = True,
        network_ready: bool = True,
        client_version: str = "7.2",
        server_version: str = "7.2",
    ) -> None:
        self.installed = installed
        self.incus_access = incus_access
        self.image_present = image_present
        self.network_ready = network_ready
        self.client_version = client_version
        self.server_version = server_version
        self.ran: list[list[str]] = []

    def which(self, command: str) -> str | None:
        assert command == "incus"
        return "/usr/bin/incus" if self.installed else None

    def run(self, argv, *, input=None, timeout_seconds=None):
        self.ran.append(argv)
        if argv == ["incus", "info"] and not self.incus_access:
            return subprocess.CompletedProcess(argv, 1, "", "permission denied")
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
        if argv == ["incus", "network", "get", "incusbr0", "ipv4.address"]:
            return subprocess.CompletedProcess(argv, 0, "10.42.0.1/24\n", "")
        if argv == ["incus", "network", "show", "incusbr0"] and not self.network_ready:
            return subprocess.CompletedProcess(argv, 1, "", "not found")
        if argv == ["incus", "image", "list", "--format", "json"]:
            images = (
                [
                    {
                        "aliases": [{"name": "dorf-codex"}],
                        "architecture": "x86_64",
                        "fingerprint": FINGERPRINT,
                        "type": "virtual-machine",
                    }
                ]
                if self.image_present
                else []
            )
            return subprocess.CompletedProcess(argv, 0, json.dumps(images), "")
        return subprocess.CompletedProcess(argv, 0, "", "")


class FakeGateway:
    def __init__(self, *, connections=None) -> None:
        self.connections = (
            connections
            if connections is not None
            else (
                ProviderConnection(
                    "personal-chatgpt",
                    "chatgpt",
                    "subscription",
                    "connected",
                ),
            )
        )
        self.required: list[str] = []

    def list_connections(self):
        return self.connections

    def require_connection(self, name):
        self.required.append(name)
        return next(connection for connection in self.connections if connection.name == name)

    def route_for_consumer(self, consumer):
        return None


class FakeDorf:
    def __init__(self, *, response: str = EXPECTED_SETUP_RESPONSE) -> None:
        self.response = response
        self.worker = None
        self.ended: list[bool] = []

    def spawn_worker(self, name):
        self.worker = SimpleNamespace(name=name, status="ready")
        return SimpleNamespace(
            worker=self.worker,
            room=SimpleNamespace(id="room-setup", status="ready"),
        )

    def message_worker(self, name, text):
        assert name == self.worker.name
        assert text == f"Reply with exactly: {EXPECTED_SETUP_RESPONSE}"
        return SimpleNamespace(message=SimpleNamespace(id="message-setup"))

    def wait_for_worker_message(self, name, *, message_id, timeout):
        assert name == self.worker.name
        assert message_id == "message-setup"
        assert timeout == 180
        return SimpleNamespace(outcome="done", response=self.response)

    def end_worker(self, name, *, interrupt=False):
        assert name == self.worker.name
        self.ended.append(interrupt)
        self.worker.status = "ended"
        return SimpleNamespace(
            worker=self.worker,
            room=SimpleNamespace(id="room-setup"),
        )

    def get_worker(self, name):
        assert name == self.worker.name
        return self.worker


def setup_runner(
    tmp_path,
    monkeypatch,
    *,
    probe=None,
    dorf=None,
    gateway=None,
    provider_connector=None,
    incus_installer=None,
    incus_access_repairer=None,
    incus_initializer=None,
    official_image_installer=USE_READY_IMAGE,
    image_progress=None,
):
    monkeypatch.setattr("dorf.core_setup.platform.system", lambda: "Linux")
    monkeypatch.setattr("dorf.core_setup.platform.machine", lambda: "x86_64")
    monkeypatch.setattr(
        "dorf.core_setup.inspect_host_capacity",
        lambda: HostCapacity(
            kvm_available=True,
            cpu_virtualization=True,
            memory_bytes=32 * GIB,
            disk_free_bytes=200 * GIB,
        ),
    )
    fake_dorf = dorf or FakeDorf()
    fake_gateway = gateway or FakeGateway()
    opened: list[tuple] = []

    def open_dorf(*args, **kwargs):
        opened.append((args, kwargs))
        return nullcontext(fake_dorf)

    if official_image_installer is USE_READY_IMAGE:

        def reuse_image(*, alias, emit, progress):
            return OfficialImageInstallResult(
                "already-ready",
                "room-image-20260731-0.150.0",
                FINGERPRINT,
                "0.150.0",
            )

        official_image_installer = reuse_image

    setup = CoreSetup(
        probe=probe or FakeProbe(),
        gateway_opener=lambda **kwargs: nullcontext(fake_gateway),
        dorf_opener=open_dorf,
        provider_connector=provider_connector,
        incus_installer=incus_installer,
        incus_access_repairer=incus_access_repairer,
        incus_initializer=incus_initializer,
        official_image_installer=official_image_installer,
        image_progress=image_progress,
        config_home=tmp_path / "config",
        temp_root=tmp_path,
    )
    return setup, fake_dorf, fake_gateway, opened


def test_setup_converges_existing_authorities_and_proves_real_worker_shape(
    tmp_path,
    monkeypatch,
) -> None:
    setup, dorf, gateway, opened = setup_runner(tmp_path, monkeypatch)
    output: list[str] = []

    result = setup.run(emit=output.append)

    assert result.provider_connection == "personal-chatgpt"
    assert result.image_fingerprint == FINGERPRINT
    assert result.profile_path == tmp_path / "config" / "dorf" / "deployment.json"
    assert load_deployment_profile(config_home=tmp_path / "config").image_fingerprint == FINGERPRINT
    assert dorf.ended == [False]
    assert gateway.required == []
    assert opened[0][1]["provider_connection"] == "personal-chatgpt"
    assert opened[0][1]["provider_gateway"] is gateway
    assert not list(tmp_path.glob("dorf-setup.*"))
    assert output == [
        "✓ Linux · x86_64",
        "✓ Hardware virtualization available · KVM",
        "✓ Host capacity · 32 GiB memory · 200 GiB free",
        "✓ Incus service available",
        "✓ Private VM network ready · incusbr0",
        "",
        "Room image",
        f"✓ dorf-codex · {FINGERPRINT[:12]} · Codex 0.150.0",
        "",
        "Model connection",
        "✓ Model connection ready · personal-chatgpt",
        "",
        "Verifying the complete Worker loop",
        "✓ Disposable Room created",
        "✓ Codex completed a real turn",
        "✓ Provider route revoked",
        "✓ Disposable Room destroyed",
        "",
        "✓ Global Dorf configuration ready",
    ]


def test_setup_stops_before_incus_when_hardware_virtualization_is_missing(
    tmp_path,
    monkeypatch,
) -> None:
    setup, _, _, opened = setup_runner(tmp_path, monkeypatch)
    monkeypatch.setattr(
        "dorf.core_setup.inspect_host_capacity",
        lambda: HostCapacity(
            kvm_available=False,
            cpu_virtualization=True,
            memory_bytes=32 * GIB,
            disk_free_bytes=200 * GIB,
        ),
    )

    with pytest.raises(CoreSetupPaused, match="Hardware virtualization"):
        setup.run(emit=lambda message: None)

    assert not opened


@pytest.mark.parametrize(
    ("memory_bytes", "disk_free_bytes", "message"),
    [
        (3 * GIB, 200 * GIB, "requires at least 4 GiB"),
        (32 * GIB, 19 * GIB, "needs more room"),
    ],
)
def test_setup_stops_before_incus_when_host_capacity_is_too_small(
    tmp_path,
    monkeypatch,
    memory_bytes,
    disk_free_bytes,
    message,
) -> None:
    setup, _, _, opened = setup_runner(tmp_path, monkeypatch)
    monkeypatch.setattr(
        "dorf.core_setup.inspect_host_capacity",
        lambda: HostCapacity(
            kvm_available=True,
            cpu_virtualization=True,
            memory_bytes=memory_bytes,
            disk_free_bytes=disk_free_bytes,
        ),
    )

    with pytest.raises(CoreSetupPaused, match=message):
        setup.run(emit=lambda output: None)

    assert not opened


def test_setup_rerun_reinspects_and_reverifies_instead_of_trusting_completion(
    tmp_path,
    monkeypatch,
) -> None:
    setup, dorf, gateway, opened = setup_runner(tmp_path, monkeypatch)

    first = setup.run(emit=lambda message: None)
    monkeypatch.setattr(
        "dorf.core_setup.save_deployment_profile",
        lambda *args, **kwargs: pytest.fail("unchanged setup rewrote its configuration"),
    )
    second = setup.run(emit=lambda message: None)

    assert second == first
    assert len(opened) == 2
    assert dorf.ended == [False, False]
    assert gateway.required == ["personal-chatgpt"]


def test_setup_connects_the_selected_provider_when_none_exists(
    tmp_path,
    monkeypatch,
) -> None:
    gateway = FakeGateway(connections=())
    connected: list[str] = []

    def connect(selected_gateway):
        assert selected_gateway is gateway
        connection = ProviderConnection(
            "personal-chatgpt",
            "chatgpt",
            "subscription",
            "connected",
        )
        gateway.connections = (connection,)
        connected.append(connection.name)
        return connection.name

    setup, _, _, _ = setup_runner(
        tmp_path,
        monkeypatch,
        gateway=gateway,
        provider_connector=connect,
    )

    result = setup.run(emit=lambda message: None)

    assert result.provider_connection == "personal-chatgpt"
    assert connected == ["personal-chatgpt"]
    assert gateway.required == ["personal-chatgpt"]


def test_setup_initializes_a_pristine_incus_daemon_then_continues(
    tmp_path,
    monkeypatch,
) -> None:
    probe = FakeProbe(network_ready=False)
    initialized: list[str] = []

    def initialize(selected_probe, config):
        assert selected_probe is probe
        initialized.append(config.network)
        probe.network_ready = True

    setup, _, _, opened = setup_runner(
        tmp_path,
        monkeypatch,
        probe=probe,
        incus_initializer=initialize,
    )

    setup.run(emit=lambda message: None)

    assert initialized == ["incusbr0"]
    assert opened


def test_setup_installs_incus_and_continues_when_access_is_already_effective(
    tmp_path,
    monkeypatch,
) -> None:
    probe = FakeProbe(installed=False)
    installed: list[bool] = []

    def install(selected_probe):
        assert selected_probe is probe
        installed.append(True)
        probe.installed = True

    setup, _, _, opened = setup_runner(
        tmp_path,
        monkeypatch,
        probe=probe,
        incus_installer=install,
    )

    setup.run(emit=lambda message: None)

    assert installed == [True]
    assert opened


def test_setup_pauses_for_a_new_login_only_when_installed_incus_is_inaccessible(
    tmp_path,
    monkeypatch,
) -> None:
    probe = FakeProbe(installed=False, incus_access=False)

    def install(selected_probe):
        assert selected_probe is probe
        probe.installed = True

    setup, _, _, opened = setup_runner(
        tmp_path,
        monkeypatch,
        probe=probe,
        incus_installer=install,
    )

    with pytest.raises(CoreSetupPaused, match="this login does not have Incus access"):
        setup.run(emit=lambda message: None)

    assert not opened


def test_setup_repairs_inaccessible_incus_then_continues(
    tmp_path,
    monkeypatch,
) -> None:
    probe = FakeProbe(incus_access=False)
    repaired: list[bool] = []

    def repair(selected_probe):
        assert selected_probe is probe
        repaired.append(True)
        probe.incus_access = True

    setup, _, _, opened = setup_runner(
        tmp_path,
        monkeypatch,
        probe=probe,
        incus_access_repairer=repair,
    )

    setup.run(emit=lambda message: None)

    assert repaired == [True]
    assert opened


def test_setup_repairs_a_stale_local_daemon_before_room_work(
    tmp_path,
    monkeypatch,
) -> None:
    probe = FakeProbe(client_version="7.2", server_version="7.0")
    repaired: list[bool] = []

    def repair(selected_probe):
        assert selected_probe is probe
        repaired.append(True)
        probe.server_version = probe.client_version

    setup, _, _, opened = setup_runner(
        tmp_path,
        monkeypatch,
        probe=probe,
        incus_access_repairer=repair,
    )

    setup.run(emit=lambda message: None)

    assert repaired == [True]
    assert opened


def test_setup_installs_the_promoted_image_before_the_real_worker_smoke(
    tmp_path,
    monkeypatch,
) -> None:
    installed: list[str] = []

    class Installer:
        def __init__(self, *, probe, temp_root):
            assert probe.image_present is False
            assert temp_root == tmp_path

        def ensure(self, *, alias, emit, progress):
            assert not installed
            assert progress is image_progress
            installed.append(alias)
            emit("Downloading verified Dorf Room image · 780 MiB")
            return OfficialImageInstallResult(
                "installed",
                "room-image-20260804-0.146.0",
                "b" * 64,
                "0.146.0",
            )

    monkeypatch.setattr("dorf.core_setup.OfficialImageInstaller", Installer)

    def image_progress(current, total):
        pass

    setup, _, _, opened = setup_runner(
        tmp_path,
        monkeypatch,
        probe=FakeProbe(image_present=False),
        official_image_installer=None,
        image_progress=image_progress,
    )
    output: list[str] = []

    result = setup.run(emit=output.append)

    assert installed == ["dorf-codex"]
    assert result.image_fingerprint == "b" * 64
    assert "Downloading verified Dorf Room image · 780 MiB" in output
    assert opened
    assert load_deployment_profile(config_home=tmp_path / "config").image_fingerprint == "b" * 64


def test_setup_pauses_before_worker_smoke_when_official_image_is_unavailable(
    tmp_path,
    monkeypatch,
) -> None:
    def install(*, alias, emit, progress):
        raise OfficialImageError("No promoted official Dorf Room image was found")

    setup, _, _, opened = setup_runner(
        tmp_path,
        monkeypatch,
        official_image_installer=install,
    )

    with pytest.raises(CoreSetupPaused, match="No promoted official") as raised:
        setup.run(emit=lambda message: None)

    assert raised.value.owner == "dorf"
    assert raised.value.classification == "possible-upstream-regression"
    assert not opened
    assert not (tmp_path / "config" / "dorf" / "deployment.json").exists()


def test_setup_failure_interrupts_the_disposable_worker_and_saves_no_profile(
    tmp_path,
    monkeypatch,
) -> None:
    dorf = FakeDorf(response="unexpected")
    setup, _, _, _ = setup_runner(tmp_path, monkeypatch, dorf=dorf)

    with pytest.raises(CoreSetupFailed, match="expected setup verification"):
        setup.run(emit=lambda message: None)

    assert dorf.ended == [True]
    assert not (tmp_path / "config" / "dorf" / "deployment.json").exists()


def test_setup_cli_has_no_required_options_and_ends_with_the_next_command(
    monkeypatch,
) -> None:
    class PassingSetup:
        def __init__(self, **kwargs):
            pass

        def run(self, *, emit):
            emit("✓ Disposable Room destroyed")
            return CoreSetupResult(
                "personal-chatgpt",
                FINGERPRINT,
                Path("/tmp/deployment.json"),
            )

    monkeypatch.setattr("dorf.cli.CoreSetup", PassingSetup)

    result = CliRunner().invoke(app, ["setup"])

    assert result.exit_code == 0
    assert "◆ Dorf" in result.output
    assert "Dorf is ready." in result.output
    assert "dorf worker spawn my-worker" in result.output
    assert "--" not in result.output


def test_setup_cli_reports_download_milestones_when_output_is_redirected(
    monkeypatch,
) -> None:
    class PassingSetup:
        def __init__(self, *, image_progress, **kwargs):
            self.image_progress = image_progress

        def run(self, *, emit):
            self.image_progress(0, 800 * 1024 * 1024)
            self.image_progress(200 * 1024 * 1024, 800 * 1024 * 1024)
            self.image_progress(800 * 1024 * 1024, 800 * 1024 * 1024)
            return CoreSetupResult(
                "personal-chatgpt",
                FINGERPRINT,
                Path("/tmp/deployment.json"),
            )

    monkeypatch.setattr("dorf.cli.CoreSetup", PassingSetup)

    result = CliRunner().invoke(app, ["setup"])

    assert result.exit_code == 0
    assert "Downloading · 25% · 200 MiB / 800 MiB" in result.output
    assert "Downloading · 100% · 800 MiB / 800 MiB" in result.output
    assert "\r" not in result.output


def test_setup_download_progress_uses_one_terminal_bar(monkeypatch) -> None:
    output: list[tuple[str, bool]] = []

    def echo(message="", *, nl=True, **kwargs):
        output.append((message, nl))

    monkeypatch.setattr("dorf.cli.typer.echo", echo)
    progress = _ImageDownloadProgress(tty=True)

    progress.update(200 * 1024 * 1024, 800 * 1024 * 1024)
    progress.update(800 * 1024 * 1024, 800 * 1024 * 1024)

    assert output[0] == ("\r  [######------------------]  25% · 200 MiB / 800 MiB", False)
    assert output[1] == ("\r  [########################] 100% · 800 MiB / 800 MiB", False)
    assert output[2] == ("", True)


def test_setup_cli_pause_is_bounded_and_explains_how_to_resume(
    tmp_path,
    monkeypatch,
) -> None:
    class PausedSetup:
        def __init__(self, **kwargs):
            pass

        def run(self, *, emit):
            raise CoreSetupPaused(
                "Incus is not installed.",
                remediation="Install Incus for this Linux distribution.",
            )

    monkeypatch.setattr("dorf.cli.CoreSetup", PausedSetup)

    result = CliRunner().invoke(
        app,
        ["setup"],
        env={"XDG_STATE_HOME": str(tmp_path / "state")},
    )

    assert result.exit_code == 1
    assert "Setup paused" in result.output
    assert "Next: Install Incus for this Linux distribution." in result.output
    assert "Then rerun: dorf setup" in result.output
    assert "Human-readable diagnostic:" in result.output
    assert "Agent-readable diagnostic:" in result.output
    bundles = list((tmp_path / "state" / "dorf" / "diagnostics").iterdir())
    assert len(bundles) == 1
    diagnostic = json.loads((bundles[0] / "diagnostic.json").read_text())
    assert diagnostic["owner"] == "dorf"
    assert diagnostic["classification"] == "configuration"


def test_setup_cli_redacts_failure_before_rendering_and_persisting(
    tmp_path,
    monkeypatch,
) -> None:
    class FailedSetup:
        def __init__(self, **kwargs):
            pass

        def run(self, *, emit):
            raise CoreSetupFailed(
                "Provider rejected api_key=sk-proj-supersecretvalue",
                owner="provider-gateway",
            )

    monkeypatch.setattr("dorf.cli.CoreSetup", FailedSetup)

    result = CliRunner().invoke(
        app,
        ["setup"],
        env={"XDG_STATE_HOME": str(tmp_path / "state")},
    )

    assert result.exit_code == 1
    assert "Setup failed" in result.output
    assert "sk-proj-supersecretvalue" not in result.output
    assert "[REDACTED]" in result.output
    bundles = list((tmp_path / "state" / "dorf" / "diagnostics").iterdir())
    combined = "\n".join(path.read_text() for path in bundles[0].iterdir())
    assert "sk-proj-supersecretvalue" not in combined


def test_setup_cli_guides_the_recommended_chatgpt_device_connection(
    monkeypatch,
) -> None:
    class SignupGateway:
        def connect_chatgpt_subscription(self, *, name, on_authorization):
            assert name == "personal-chatgpt"
            on_authorization(
                DeviceAuthorization(
                    "https://auth.openai.com/codex/device",
                    "ABCD-EFGH",
                )
            )
            return ProviderConnection(
                name,
                "chatgpt",
                "subscription",
                "connected",
            )

    class OnboardingSetup:
        def __init__(self, *, provider_connector, **kwargs):
            self.provider_connector = provider_connector

        def run(self, *, emit):
            provider = self.provider_connector(SignupGateway())
            return CoreSetupResult(
                provider,
                FINGERPRINT,
                Path("/tmp/deployment.json"),
            )

    monkeypatch.setattr("dorf.cli.CoreSetup", OnboardingSetup)

    result = CliRunner().invoke(app, ["setup"], input="\n")

    assert result.exit_code == 0
    assert "ChatGPT subscription · recommended" in result.output
    assert "Open: https://auth.openai.com/codex/device" in result.output
    assert "Code: ABCD-EFGH" in result.output
    assert "✓ Connected · personal-chatgpt" in result.output
    assert "Provider: personal-chatgpt" in result.output
