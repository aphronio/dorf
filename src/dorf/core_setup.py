"""Concrete convergence path for the built-in local Dorf deployment."""

from __future__ import annotations

import json
import platform
import secrets
import tempfile
from collections.abc import Callable
from contextlib import AbstractContextManager
from dataclasses import dataclass
from pathlib import Path
from typing import Any

from dorf import Dorf
from dorf.adapters.environments import (
    IncusConfig,
    IncusRunnerProbe,
    incus_bridge_ipv4,
)
from dorf.deployment_profile import (
    DeploymentProfile,
    DeploymentProfileError,
    deployment_profile_path,
    load_optional_deployment_profile,
    save_deployment_profile,
)
from dorf.host_setup import (
    GIB,
    MINIMUM_HOST_DISK_FREE_BYTES,
    MINIMUM_HOST_MEMORY_BYTES,
    HostSetupError,
    inspect_host_capacity,
    inspect_incus_versions,
)
from dorf.provider_gateway import ProviderGateway, ProviderGatewayError
from dorf.sdk import EnvironmentPrerequisitesError
from dorf.setup_diagnostics import SetupDiagnostic

EXPECTED_SETUP_RESPONSE = "dorf setup ready"


class CoreSetupPaused(RuntimeError):
    """Setup cannot continue until the user completes one bounded action."""

    def __init__(
        self,
        message: str,
        *,
        remediation: str,
        owner: str = "dorf",
        classification: str = "configuration",
        expected: str = "Dorf setup can continue safely.",
        approval_required_actions: tuple[str, ...] = (),
    ) -> None:
        self.remediation = remediation
        self.owner = owner
        self.classification = classification
        self.expected = expected
        self.approval_required_actions = approval_required_actions
        super().__init__(message)

    def to_diagnostic(self) -> SetupDiagnostic:
        return SetupDiagnostic(
            status="paused",
            owner=self.owner,
            classification=self.classification,
            summary=str(self),
            observed=(str(self),),
            expected=(self.expected,),
            safe_actions=(self.remediation,),
            approval_required_actions=self.approval_required_actions,
        )


class CoreSetupFailed(RuntimeError):
    """The complete Worker verification failed or did not clean up."""

    def __init__(
        self,
        message: str,
        *,
        owner: str = "dorf",
        classification: str = "possible-upstream-regression",
        expected: str = "The disposable Worker verification completes and cleans up exactly.",
    ) -> None:
        self.owner = owner
        self.classification = classification
        self.expected = expected
        super().__init__(message)

    def to_diagnostic(self) -> SetupDiagnostic:
        return SetupDiagnostic(
            status="failed",
            owner=self.owner,
            classification=self.classification,
            summary=str(self),
            observed=(str(self),),
            expected=(self.expected,),
            safe_actions=(
                "Rerun dorf setup once.",
                "If the same code repeats, review and report the diagnostic bundle.",
            ),
        )


@dataclass(frozen=True)
class CoreSetupResult:
    provider_connection: str
    image_fingerprint: str
    profile_path: Path


class CoreSetup:
    """Inspect real authorities, converge defaults, and prove one disposable Worker."""

    def __init__(
        self,
        *,
        probe: IncusRunnerProbe | None = None,
        gateway_opener: Callable[..., AbstractContextManager[Any]] | None = None,
        dorf_opener: Callable[..., AbstractContextManager[Any]] | None = None,
        provider_connector: Callable[[Any], str] | None = None,
        incus_installer: Callable[[IncusRunnerProbe], None] | None = None,
        incus_access_repairer: Callable[[IncusRunnerProbe], None] | None = None,
        incus_initializer: Callable[[IncusRunnerProbe, IncusConfig], None] | None = None,
        config_home: Path | None = None,
        temp_root: Path | None = None,
    ) -> None:
        self._probe = probe or IncusRunnerProbe()
        self._gateway_opener = gateway_opener or ProviderGateway.open
        self._dorf_opener = dorf_opener or Dorf.open
        self._provider_connector = provider_connector
        self._incus_installer = incus_installer
        self._incus_access_repairer = incus_access_repairer
        self._incus_initializer = incus_initializer
        self._config_home = config_home
        self._temp_root = temp_root

    def run(self, *, emit: Callable[[str], None]) -> CoreSetupResult:
        architecture = _supported_host_architecture()
        emit(f"✓ Linux · {architecture}")
        self._require_host_capacity(emit)

        profile = self._load_profile()
        config = profile.incus if profile is not None else IncusConfig()
        self._require_incus(config)
        emit("✓ Incus service available")
        emit(f"✓ Private VM network ready · {config.network}")

        emit("")
        emit("Room image")
        fingerprint = self._local_image_fingerprint(config)
        emit(f"✓ {config.template} · {fingerprint[:12]}")

        emit("")
        emit("Model connection")
        try:
            bridge_address = incus_bridge_ipv4(config.network, probe=self._probe)
        except RuntimeError as error:
            raise CoreSetupPaused(
                f"Private Incus network {config.network} has no usable bridge address.",
                remediation="Repair the private Incus network address.",
                owner="incus",
                expected="The selected Incus bridge has a private IPv4 address.",
            ) from error
        try:
            with self._gateway_opener(bind_address=bridge_address) as gateway:
                provider_connection = self._select_provider_connection(
                    gateway,
                    profile=profile,
                )
                emit(f"✓ Model connection ready · {provider_connection}")
                emit("")
                emit("Verifying the complete Worker loop")
                self._verify_worker(
                    gateway,
                    config=config,
                    provider_connection=provider_connection,
                    emit=emit,
                )
        except CoreSetupPaused:
            raise
        except ProviderGatewayError as error:
            remediation = getattr(error, "remediation", None)
            raise CoreSetupPaused(
                f"Model connection is not ready: {error}",
                remediation=(
                    remediation
                    if isinstance(remediation, str) and remediation
                    else "Run: dorf provider status <name>"
                ),
                owner="provider-gateway",
                expected="The selected Provider Connection is connected and usable.",
            ) from error

        updated = DeploymentProfile(
            provider_connection=provider_connection,
            incus=config,
            image_fingerprint=fingerprint,
        )
        profile_path = deployment_profile_path(config_home=self._config_home)
        if profile != updated:
            try:
                profile_path = save_deployment_profile(
                    updated,
                    config_home=self._config_home,
                )
            except (DeploymentProfileError, OSError) as error:
                raise CoreSetupFailed(
                    f"Could not save the global Dorf configuration: {error}",
                    classification="configuration",
                    expected="The global deployment profile can be written with mode 0600.",
                ) from error
        emit("")
        emit("✓ Global Dorf configuration ready")
        return CoreSetupResult(
            provider_connection=provider_connection,
            image_fingerprint=fingerprint,
            profile_path=profile_path,
        )

    def _require_host_capacity(self, emit: Callable[[str], None]) -> None:
        try:
            capacity = inspect_host_capacity()
        except HostSetupError as error:
            raise CoreSetupPaused(
                f"Dorf could not inspect host virtualization and capacity: {error}",
                remediation="Repair access to the reported Linux host fact.",
                owner="host",
                expected="Linux virtualization, memory, and disk facts are readable.",
            ) from error
        if not capacity.kvm_available or not capacity.cpu_virtualization:
            raise CoreSetupPaused(
                "Hardware virtualization is not available to the local Incus service.",
                remediation="Enable CPU virtualization and make /dev/kvm available.",
                owner="host",
                classification="unsupported",
                expected="CPU virtualization is enabled and /dev/kvm is a character device.",
            )
        if capacity.memory_bytes < MINIMUM_HOST_MEMORY_BYTES:
            raise CoreSetupPaused(
                (
                    f"This host has {capacity.memory_bytes // GIB} GiB memory; "
                    f"Dorf requires at least {MINIMUM_HOST_MEMORY_BYTES // GIB} GiB."
                ),
                remediation="Use a host with enough memory for one local Room.",
                owner="host",
                classification="unsupported",
                expected=(
                    f"At least {MINIMUM_HOST_MEMORY_BYTES // GIB} GiB total memory is available."
                ),
            )
        if capacity.disk_free_bytes < MINIMUM_HOST_DISK_FREE_BYTES:
            raise CoreSetupPaused(
                (
                    f"This host has {capacity.disk_free_bytes // GIB} GiB free on /; "
                    "Dorf needs more room for its VM image and workspace."
                ),
                remediation=(f"Free at least {MINIMUM_HOST_DISK_FREE_BYTES // GIB} GiB on /."),
                owner="host",
                expected=(f"At least {MINIMUM_HOST_DISK_FREE_BYTES // GIB} GiB is free on /."),
            )
        emit("✓ Hardware virtualization available · KVM")
        emit(
            "✓ Host capacity · "
            f"{capacity.memory_bytes // GIB} GiB memory · "
            f"{capacity.disk_free_bytes // GIB} GiB free"
        )

    def _load_profile(self) -> DeploymentProfile | None:
        try:
            return load_optional_deployment_profile(config_home=self._config_home)
        except DeploymentProfileError as error:
            raise CoreSetupPaused(
                f"Global Dorf configuration is invalid: {error}",
                remediation=(
                    "Repair or remove the reported deployment.json, "
                    "preserving any Provider Connection name you still use."
                ),
                expected="The global deployment profile is valid JSON with supported fields.",
            ) from error

    def _require_incus(self, config: IncusConfig) -> None:
        installed_now = False
        if self._probe.which("incus") is None:
            if self._incus_installer is not None:
                self._incus_installer(self._probe)
                installed_now = self._probe.which("incus") is not None
            if not installed_now:
                raise CoreSetupPaused(
                    "Incus is not installed. Dorf uses it to create isolated local Rooms.",
                    remediation="Install Incus for this Linux distribution.",
                    owner="host",
                    expected="The supported Incus command and local service are installed.",
                )
        server = self._probe.run(["incus", "info"], timeout_seconds=30)
        if server.returncode != 0:
            if self._incus_access_repairer is not None:
                self._incus_access_repairer(self._probe)
                server = self._probe.run(["incus", "info"], timeout_seconds=30)
        if server.returncode != 0:
            if installed_now:
                raise CoreSetupPaused(
                    "Incus was installed, but this login does not have Incus access yet.",
                    remediation="Sign out and back in so incus-admin membership takes effect.",
                    owner="host",
                    expected="The current login can operate the local Incus service.",
                )
            raise CoreSetupPaused(
                "The current user cannot operate the local Incus service.",
                remediation="Grant this user access to the local Incus service.",
                owner="host",
                expected="The current login can operate the local Incus service.",
            )
        try:
            versions = inspect_incus_versions(self._probe)
            if not versions.aligned and self._incus_access_repairer is not None:
                self._incus_access_repairer(self._probe)
                versions = inspect_incus_versions(self._probe)
        except HostSetupError as error:
            raise CoreSetupPaused(
                f"Dorf could not inspect the local Incus version pair: {error}",
                remediation="Run `incus version` and repair the reported local service failure.",
                owner="incus",
                classification="compatibility",
                expected="The local Incus client and daemon report their versions.",
            ) from error
        if not versions.aligned:
            raise CoreSetupPaused(
                (
                    f"The Incus client is {versions.client}, but the local daemon is "
                    f"{versions.server}."
                ),
                remediation="Restart the local Incus service to activate its package update.",
                owner="incus",
                classification="compatibility",
                expected="The local Incus client and daemon use the same installed version.",
                approval_required_actions=("Restart the local Incus service.",),
            )
        network = self._probe.run(
            ["incus", "network", "show", config.network],
            timeout_seconds=30,
        )
        if network.returncode != 0:
            if self._incus_initializer is not None:
                self._incus_initializer(self._probe, config)
                network = self._probe.run(
                    ["incus", "network", "show", config.network],
                    timeout_seconds=30,
                )
            if network.returncode == 0:
                return
            raise CoreSetupPaused(
                f"Private Incus network {config.network} is not ready.",
                remediation=f"Initialize the private Incus network {config.network}.",
                owner="incus",
                expected=f"Managed private network {config.network} exists.",
            )

    def _local_image_fingerprint(self, config: IncusConfig) -> str:
        listed = self._probe.run(
            ["incus", "image", "list", "--format", "json"],
            timeout_seconds=30,
        )
        if listed.returncode != 0:
            raise CoreSetupPaused(
                "Dorf could not inspect local Incus images.",
                remediation="Run `incus image list` and repair the reported Incus failure.",
                owner="incus",
                expected="Incus returns bounded local image metadata.",
            )
        try:
            images = json.loads(listed.stdout)
        except json.JSONDecodeError as error:
            raise CoreSetupPaused(
                "Incus returned invalid image metadata.",
                remediation="Run `incus image list` and repair the reported Incus failure.",
                owner="incus",
                classification="compatibility",
                expected="Incus returns image metadata as a JSON list.",
            ) from error
        if not isinstance(images, list):
            raise CoreSetupPaused(
                "Incus returned invalid image metadata.",
                remediation="Run `incus image list` and repair the reported Incus failure.",
                owner="incus",
                classification="compatibility",
                expected="Incus returns image metadata as a JSON list.",
            )
        matches = [
            image
            for image in images
            if isinstance(image, dict)
            and any(
                isinstance(alias, dict) and alias.get("name") == config.template
                for alias in image.get("aliases", [])
            )
        ]
        if not matches:
            raise CoreSetupPaused(
                (
                    f"Dorf Codex image {config.template} is not installed. "
                    "Public image download is not active in this pre-release build."
                ),
                remediation=(
                    "Use an existing validated local image, or rerun after public "
                    "image distribution is activated."
                ),
                owner="dorf",
                expected=(f"One validated x86_64 VM image is available as {config.template}."),
            )
        if len(matches) != 1:
            raise CoreSetupFailed(
                f"Incus returned multiple images for alias {config.template}",
                owner="incus",
                classification="compatibility",
                expected=f"Alias {config.template} resolves to exactly one image.",
            )
        image = matches[0]
        if image.get("architecture") not in {"x86_64", "amd64"}:
            raise CoreSetupPaused(
                f"Incus image {config.template} is not an x86_64 image.",
                remediation="Install a supported Dorf Codex VM image.",
                owner="dorf",
                classification="compatibility",
                expected="The selected Room image uses the x86_64 architecture.",
            )
        if image.get("type") != "virtual-machine":
            raise CoreSetupPaused(
                f"Incus image {config.template} is not a VM image.",
                remediation="Install a supported Dorf Codex VM image.",
                owner="dorf",
                classification="compatibility",
                expected="The selected Room image has type virtual-machine.",
            )
        fingerprint = image.get("fingerprint")
        if (
            not isinstance(fingerprint, str)
            or len(fingerprint) != 64
            or any(character not in "0123456789abcdef" for character in fingerprint)
        ):
            raise CoreSetupFailed(
                f"Incus image {config.template} has an invalid fingerprint",
                owner="incus",
                classification="compatibility",
                expected="Incus returns a lowercase 64-character image fingerprint.",
            )
        return fingerprint

    def _select_provider_connection(
        self,
        gateway: Any,
        *,
        profile: DeploymentProfile | None,
    ) -> str:
        if profile is not None:
            gateway.require_connection(profile.provider_connection)
            return profile.provider_connection
        connected = [
            connection
            for connection in gateway.list_connections()
            if connection.status == "connected"
        ]
        if not connected:
            if self._provider_connector is not None:
                name = self._provider_connector(gateway)
                gateway.require_connection(name)
                return name
            raise CoreSetupPaused(
                "No connected model provider is available.",
                remediation=(
                    "Run: dorf provider connect chatgpt --subscription --name personal-chatgpt"
                ),
                owner="provider-gateway",
                expected="One selected Provider Connection is connected.",
            )
        if len(connected) > 1:
            raise CoreSetupPaused(
                "More than one connected model provider is available and no default is selected.",
                remediation="Connect or select one default Provider Connection, then rerun setup.",
                owner="provider-gateway",
                expected="Exactly one Provider Connection is selected as the default.",
            )
        return connected[0].name

    def _verify_worker(
        self,
        gateway: Any,
        *,
        config: IncusConfig,
        provider_connection: str,
        emit: Callable[[str], None],
    ) -> None:
        worker_name = f"setup-{secrets.token_hex(4)}"
        try:
            with tempfile.TemporaryDirectory(
                prefix="dorf-setup.",
                dir=self._temp_root,
            ) as directory:
                database_path = Path(directory) / "runtime.db"
                with self._dorf_opener(
                    database_path,
                    environment_config=config,
                    provider_connection=provider_connection,
                    provider_gateway=gateway,
                ) as dorf:
                    binding = None
                    try:
                        binding = dorf.spawn_worker(worker_name)
                        emit("✓ Disposable Room created")
                        receipt = dorf.message_worker(
                            worker_name,
                            f"Reply with exactly: {EXPECTED_SETUP_RESPONSE}",
                        )
                        result = dorf.wait_for_worker_message(
                            worker_name,
                            message_id=receipt.message.id,
                            timeout=180,
                        )
                        if result.outcome != "done" or result.response != EXPECTED_SETUP_RESPONSE:
                            raise CoreSetupFailed(
                                "Codex did not complete the expected setup verification turn",
                                owner="codex",
                            )
                        emit("✓ Codex completed a real turn")
                        ended = dorf.end_worker(worker_name)
                        if ended.worker.status != "ended" or ended.room is None:
                            raise CoreSetupFailed(
                                "Setup verification did not destroy its disposable Room",
                                expected="The disposable Worker and Room are ended exactly.",
                            )
                        consumer = f"room:{binding.room.id}"
                        if gateway.route_for_consumer(consumer) is not None:
                            raise CoreSetupFailed(
                                "Setup verification did not revoke its Provider Gateway route",
                                expected="The disposable Room inference route is absent.",
                            )
                        emit("✓ Provider route revoked")
                        emit("✓ Disposable Room destroyed")
                    finally:
                        worker = dorf.get_worker(worker_name)
                        if worker is not None and worker.status != "ended":
                            dorf.end_worker(worker_name, interrupt=True)
                        if binding is not None:
                            consumer = f"room:{binding.room.id}"
                            if gateway.route_for_consumer(consumer) is not None:
                                raise CoreSetupFailed(
                                    "Setup verification left a Provider Gateway route behind",
                                    expected="The disposable Room inference route is absent.",
                                )
        except (CoreSetupFailed, ProviderGatewayError):
            raise
        except EnvironmentPrerequisitesError as error:
            raise CoreSetupPaused(
                "The Room environment is not ready: " + "; ".join(error.failures),
                remediation="Repair the reported Incus prerequisite.",
                owner="incus",
                expected="Incus can launch the configured VM image on the private network.",
            ) from error
        except (OSError, RuntimeError, ValueError) as error:
            raise CoreSetupFailed(
                f"Worker verification failed: {error}",
            ) from error


def _supported_host_architecture() -> str:
    if platform.system() != "Linux":
        raise CoreSetupPaused(
            f"This Dorf build does not support {platform.system()} hosts.",
            remediation="Use a supported x86_64 Linux host.",
            owner="host",
            classification="unsupported",
            expected="Dorf setup runs on a supported x86_64 Linux host.",
        )
    architecture = platform.machine().lower()
    if architecture not in {"x86_64", "amd64"}:
        raise CoreSetupPaused(
            f"This Dorf build does not support {architecture} hosts.",
            remediation="Use a supported x86_64 Linux host.",
            owner="host",
            classification="unsupported",
            expected="Dorf setup runs on a supported x86_64 Linux host.",
        )
    return "x86_64"
