"""Concrete lifecycle and local HTTP behavior for the pinned broker backend."""

from __future__ import annotations

import fcntl
import hashlib
import ipaddress
import json
import os
import platform
import re
import signal
import subprocess
import tarfile
import time
import urllib.error
import urllib.request
from collections.abc import Callable
from dataclasses import dataclass, field
from pathlib import Path
from typing import Literal

PINNED_BACKEND_VERSION = "7.2.104"
DEFAULT_PORT = 8317
_RELEASE_REPOSITORY = "router-for-me/CLIProxyAPI"
_LINUX_RELEASES = {
    "aarch64": (
        "CLIProxyAPI_7.2.104_linux_aarch64.tar.gz",
        "d77647b161eb9af6c117200c4ce439a845a846acf0e8ab57420aff38989b84f5",
    ),
    "x86_64": (
        "CLIProxyAPI_7.2.104_linux_amd64.tar.gz",
        "993babb37b6de831600f0eb31527ca0f938337e1d1f837d5cf846263affa9724",
    ),
}


class ProviderGatewayError(RuntimeError):
    """A provider gateway operation failed without exposing backend details."""

    needs_human = False
    remediation = "Run: dorf doctor"


class GatewayUnavailableError(ProviderGatewayError):
    """The local provider gateway could not become ready."""


class ProviderConnectionNotFoundError(ProviderGatewayError):
    """The selected upstream provider connection does not exist."""

    needs_human = True

    def __init__(self, connection_name: str) -> None:
        self.connection_name = connection_name
        self.remediation = "Run: dorf provider connect --help"
        super().__init__(f"Provider connection not found: {connection_name}")


class ProviderSelectionUnsupportedError(ProviderGatewayError):
    """The current concrete backend cannot enforce the selected connection."""

    needs_human = True
    remediation = "Disconnect unused Provider Connections and retry"


class ProviderAuthenticationError(ProviderGatewayError):
    """A provider connection could not complete upstream authentication."""

    needs_human = True

    def __init__(self, connection_name: str) -> None:
        self.connection_name = connection_name
        self.remediation = _reconnect_remediation(
            "chatgpt",
            "subscription",
            connection_name,
        )
        super().__init__(f"Could not authenticate {connection_name}; run provider connect again")


class ProviderAuthenticationStaleError(ProviderGatewayError):
    """A named provider connection must be authenticated again."""

    needs_human = True

    def __init__(
        self,
        connection_name: str,
        *,
        provider: str,
        auth_mode: str,
    ) -> None:
        self.connection_name = connection_name
        self.remediation = _reconnect_remediation(
            provider,
            auth_mode,
            connection_name,
        )
        super().__init__(f"Provider connection needs authentication: {connection_name}")


class ProviderUpstreamUnavailableError(ProviderGatewayError):
    """A provider is temporarily unavailable through an authenticated connection."""

    needs_human = False

    def __init__(self, connection_name: str) -> None:
        self.connection_name = connection_name
        self.remediation = f"Try {connection_name} again later"
        super().__init__(f"Provider is temporarily unavailable: {connection_name}")


class ConsumerWireIncompatibleError(ProviderGatewayError):
    """The selected provider connection cannot serve the requested consumer wire."""

    needs_human = False

    def __init__(self, connection_name: str, requested_wire: str) -> None:
        self.connection_name = connection_name
        self.requested_wire = requested_wire
        self.remediation = "Use a Responses API consumer"
        super().__init__(
            f"Provider connection {connection_name} is incompatible with "
            f"consumer wire {requested_wire}"
        )


@dataclass(frozen=True)
class GatewayHealth:
    """Sanitized health of the shared local provider authority."""

    status: Literal["ready", "stopped", "unavailable"]
    backend_present: bool
    backend_version: str
    bind_addresses: tuple[str, ...]
    has_provider_connection: bool


@dataclass(frozen=True)
class ProviderConnection:
    """Sanitized upstream authentication owned by the provider gateway."""

    name: str
    provider: Literal["chatgpt", "openai", "deepseek"]
    auth_mode: Literal["subscription", "api_key"]
    status: Literal[
        "connected",
        "authentication_stale",
        "upstream_unavailable",
        "broker_unavailable",
    ]
    remediation: str | None = None
    plan: str | None = None


@dataclass(frozen=True)
class DeviceAuthorization:
    """A short-lived challenge that a person completes outside Dorf."""

    verification_url: str
    user_code: str = field(repr=False)


@dataclass(frozen=True)
class InferenceRoute:
    """A revocable broker-local credential for one inference consumer."""

    id: str
    connection_name: str
    base_url: str
    wire_api: Literal["responses"]
    api_key: str = field(repr=False)


class ProviderGateway:
    """Open and supervise the shared local provider authority."""

    def __init__(
        self,
        state_path: Path,
        executable_path: Path,
        port: int,
        *,
        manages_executable: bool,
        bind_address: str,
    ) -> None:
        self._state_path = state_path
        self._executable_path = executable_path.resolve()
        self._manages_executable = manages_executable
        self._port = port
        self._bind_address = bind_address
        self._config_path = state_path / "broker.yaml"
        self._pid_path = state_path / "broker.pid"
        self._lock_path = state_path / "broker.lock"
        self._log_path = state_path / "broker.log"
        self._auth_path = state_path / "auth"
        self._credentials_path = state_path / "credentials"
        self._connections_path = state_path / "connections.json"
        self._routes_path = state_path / "routes.json"
        self._authority_path = state_path / "authority.json"

    @classmethod
    def open(
        cls,
        state_path: Path | None = None,
        *,
        executable_path: Path | None = None,
        port: int = DEFAULT_PORT,
        bind_address: str | None = None,
    ) -> ProviderGateway:
        """Open the facade without starting or stopping the persistent broker."""
        if not 1 <= port <= 65535:
            raise ValueError("port must be between 1 and 65535")
        state_path = state_path or _default_state_path()
        if bind_address is None:
            bind_address = _read_configured_bind_address(state_path / "broker.yaml") or "127.0.0.1"
        try:
            parsed_bind_address = ipaddress.ip_address(bind_address)
        except ValueError:
            raise ValueError("bind_address must be a local IPv4 address") from None
        if not isinstance(parsed_bind_address, ipaddress.IPv4Address) or (
            parsed_bind_address.is_unspecified
            or parsed_bind_address.is_multicast
            or parsed_bind_address.is_link_local
            or (not parsed_bind_address.is_loopback and not parsed_bind_address.is_private)
        ):
            raise ValueError("bind_address must be a local IPv4 address")
        manages_executable = executable_path is None
        executable_path = executable_path or (
            state_path / "bin" / PINNED_BACKEND_VERSION / "cli-proxy-api"
        )
        return cls(
            state_path,
            executable_path,
            port,
            manages_executable=manages_executable,
            bind_address=str(parsed_bind_address),
        )

    def close(self) -> None:
        """Close this short-lived facade without stopping the shared broker."""

    def __enter__(self) -> ProviderGateway:
        return self

    def __exit__(self, *exc_info: object) -> None:
        self.close()

    def ensure_ready(self) -> GatewayHealth:
        """Start or reconnect to the one pinned local broker."""
        with self._locked():
            if self._manages_executable and not self._executable_path.exists():
                self._install_executable()
            self._verify_executable()
            pid = self._read_pid()
            if pid is not None and self._process_matches(pid):
                if not self._listener_configuration_matches():
                    self._terminate_process(pid)
                    pid = None
                elif self._probe_ready():
                    return self._ready_health()
            if pid is not None and not self._process_alive(pid):
                self._pid_path.unlink(missing_ok=True)
            elif pid is not None and self._process_matches(pid):
                raise GatewayUnavailableError("Provider gateway is running but unavailable")
            elif pid is not None:
                self._pid_path.unlink(missing_ok=True)

            self._write_config(self._read_routes(), self._read_authority())
            process = self._start_process()
            self._write_private_text(self._pid_path, f"{process.pid}\n")
            deadline = time.monotonic() + 5
            while time.monotonic() < deadline:
                if process.poll() is not None:
                    self._pid_path.unlink(missing_ok=True)
                    raise GatewayUnavailableError("Provider gateway failed to start")
                if self._probe_ready():
                    return self._ready_health()
                time.sleep(0.05)
            raise GatewayUnavailableError("Provider gateway did not become ready")

    def health(self) -> GatewayHealth:
        """Inspect bounded gateway health without installing or starting it."""
        if not self._state_path.exists():
            return self._health("stopped")
        with self._locked():
            pid = self._read_pid()
            if pid is None or not self._process_matches(pid):
                status = "stopped"
            elif self._probe_ready():
                status = "ready"
            else:
                status = "unavailable"
            return self._health(status)

    def list_connections(self) -> tuple[ProviderConnection, ...]:
        """List connected providers without exposing credential metadata."""
        with self._locked():
            return self._connections_unlocked()

    def connection_status(self, name: str) -> ProviderConnection:
        """Inspect one durable named connection without exposing credential metadata."""
        name = _validate_connection_name(name)
        with self._locked():
            record = next(
                (item for item in self._read_connections() if item["name"] == name),
                None,
            )
            if record is None:
                raise ProviderConnectionNotFoundError(name)
            credential_path = self._credential_path(record)
            if not credential_path.is_file():
                return ProviderConnection(
                    name=name,
                    provider=record["provider"],
                    auth_mode=record["auth_mode"],
                    status="authentication_stale",
                    remediation=_reconnect_remediation(
                        record["provider"],
                        record["auth_mode"],
                        name,
                    ),
                )
        try:
            self.ensure_ready()
            if record["auth_mode"] == "api_key":
                return ProviderConnection(
                    name=name,
                    provider=record["provider"],
                    auth_mode=record["auth_mode"],
                    status="connected",
                )
            entry = self._connection_backend_status(record)
        except GatewayUnavailableError:
            return ProviderConnection(
                name=name,
                provider=record["provider"],
                auth_mode=record["auth_mode"],
                status="broker_unavailable",
                remediation="Run: dorf doctor",
            )
        if entry is None or entry.get("status") == "disabled":
            if entry is not None and entry.get("unavailable") is True:
                return ProviderConnection(
                    name=name,
                    provider=record["provider"],
                    auth_mode=record["auth_mode"],
                    status="upstream_unavailable",
                    remediation=f"Try {name} again later",
                )
            return ProviderConnection(
                name=name,
                provider=record["provider"],
                auth_mode=record["auth_mode"],
                status="authentication_stale",
                remediation=_reconnect_remediation(
                    record["provider"],
                    record["auth_mode"],
                    name,
                ),
            )
        if entry.get("unavailable") is True:
            return ProviderConnection(
                name=name,
                provider=record["provider"],
                auth_mode=record["auth_mode"],
                status="upstream_unavailable",
                remediation=f"Try {name} again later",
            )
        return ProviderConnection(
            name=name,
            provider=record["provider"],
            auth_mode=record["auth_mode"],
            status="connected",
            plan=_safe_plan(entry),
        )

    def require_connection(self, name: str) -> ProviderConnection:
        """Require one connection to be ready for a new inference consumer."""
        connection = self.connection_status(name)
        if connection.status == "authentication_stale":
            raise ProviderAuthenticationStaleError(
                name,
                provider=connection.provider,
                auth_mode=connection.auth_mode,
            )
        if connection.status == "upstream_unavailable":
            raise ProviderUpstreamUnavailableError(name)
        if connection.status == "broker_unavailable":
            raise GatewayUnavailableError(
                f"Provider gateway is unavailable; {connection.remediation}"
            )
        return connection

    def connect_openai_api_key(self, *, name: str, api_key: str) -> ProviderConnection:
        return self._connect_api_key(name, api_key, "openai")

    def connect_deepseek_api_key(self, *, name: str, api_key: str) -> ProviderConnection:
        return self._connect_api_key(name, api_key, "deepseek")

    def _connect_api_key(
        self,
        name: str,
        api_key: str,
        provider: Literal["openai", "deepseek"],
    ) -> ProviderConnection:
        name = _validate_connection_name(name)
        api_key = api_key.strip()
        if not api_key:
            raise ValueError("api_key cannot be empty")
        secret_name = f"{provider}-{hashlib.sha256(name.encode()).hexdigest()[:16]}.key"
        secret_path = self._credentials_path / secret_name
        with self._locked():
            previous_records = self._read_connections()
            existing = next((record for record in previous_records if record["name"] == name), None)
            if existing is not None and (
                existing["provider"] != provider or existing["auth_mode"] != "api_key"
            ):
                raise ValueError(f"Provider connection already exists: {name}")
            try:
                previous_secret = secret_path.read_text()
            except FileNotFoundError:
                previous_secret = None
            except OSError:
                raise GatewayUnavailableError(
                    "Provider connection authentication is unavailable"
                ) from None
            self._credentials_path.mkdir(parents=True, exist_ok=True, mode=0o700)
            self._credentials_path.chmod(0o700)
            self._write_private_text(secret_path, f"{api_key}\n")
            records = [record for record in previous_records if record["name"] != name]
            records.append(
                {
                    "name": name,
                    "provider": provider,
                    "auth_mode": "api_key",
                    "credential_ref": secret_name,
                }
            )
            self._write_connections(records)
        try:
            self.shutdown()
            self.ensure_ready()
        except ProviderGatewayError:
            with self._locked():
                self._write_connections(previous_records)
                if previous_secret is None:
                    secret_path.unlink(missing_ok=True)
                else:
                    self._write_private_text(secret_path, previous_secret)
            try:
                self.ensure_ready()
            except ProviderGatewayError:
                pass
            raise
        return ProviderConnection(name, provider, "api_key", "connected")

    def connect_chatgpt_subscription(
        self,
        *,
        name: str,
        on_authorization: Callable[[DeviceAuthorization], None],
    ) -> ProviderConnection:
        """Connect one named ChatGPT subscription through a device challenge."""
        name = _validate_connection_name(name)
        adopted = self._adopt_single_unnamed_subscription(name)
        if adopted is not None:
            self.shutdown()
            self.ensure_ready()
            self._await_models_available(self._read_authority()["guard_key"])
            return adopted
        self.ensure_ready()
        before = self._snapshot_auth_files()
        try:
            process = subprocess.Popen(
                [
                    self._executable_path,
                    "-config",
                    self._config_path,
                    "-codex-device-login",
                    "-no-browser",
                ],
                cwd=self._state_path,
                stdin=subprocess.DEVNULL,
                stdout=subprocess.PIPE,
                stderr=subprocess.STDOUT,
                text=True,
            )
        except OSError:
            raise ProviderAuthenticationError(name) from None

        verification_url: str | None = None
        user_code: str | None = None
        saw_success = False
        assert process.stdout is not None
        for line in process.stdout:
            stripped = line.strip()
            if stripped.startswith("Codex device URL: "):
                verification_url = stripped.removeprefix("Codex device URL: ").strip()
            elif stripped.startswith("Codex device code: "):
                user_code = stripped.removeprefix("Codex device code: ").strip()
            elif stripped == "Codex device authentication successful!":
                saw_success = True
            if verification_url and user_code:
                on_authorization(
                    DeviceAuthorization(
                        verification_url=verification_url,
                        user_code=user_code,
                    )
                )
                verification_url = None
                user_code = None
        return_code = process.wait()
        changed = [
            path
            for path, modified in self._snapshot_auth_files().items()
            if before.get(path) != modified
        ]
        if return_code != 0 or not saw_success or len(changed) != 1:
            raise ProviderAuthenticationError(name)

        credential_name = f"codex-dorf-{hashlib.sha256(name.encode()).hexdigest()[:16]}.json"
        credential_path = self._auth_path / credential_name
        with self._locked():
            records = self._read_connections()
            existing = next(
                (record for record in records if record["name"] == name),
                None,
            )
            if existing is not None and (
                existing["provider"] != "chatgpt" or existing["auth_mode"] != "subscription"
            ):
                raise ValueError(f"Provider connection already exists: {name}")
            os.replace(changed[0], credential_path)
            credential_path.chmod(0o600)
            records = [record for record in records if record["name"] != name]
            records.append(
                {
                    "name": name,
                    "provider": "chatgpt",
                    "auth_mode": "subscription",
                    "credential_ref": credential_name,
                }
            )
            self._write_connections(records)
        self.shutdown()
        self.ensure_ready()
        self._await_models_available(self._read_authority()["guard_key"])
        return ProviderConnection(
            name=name,
            provider="chatgpt",
            auth_mode="subscription",
            status="connected",
        )

    def _adopt_single_unnamed_subscription(
        self,
        name: str,
    ) -> ProviderConnection | None:
        with self._locked():
            records = self._read_connections()
            if any(record["name"] == name for record in records):
                return None
            registered = {
                record["credential_ref"]
                for record in records
                if record["auth_mode"] == "subscription"
            }
            candidates = [
                path
                for path in self._auth_path.glob("codex-*.json")
                if path.name not in registered and path.is_file() and not path.is_symlink()
            ]
            if not candidates:
                return None
            if len(candidates) != 1:
                raise ProviderSelectionUnsupportedError(
                    "Multiple unnamed Provider Connections require explicit cleanup"
                )
            credential_name = f"codex-dorf-{hashlib.sha256(name.encode()).hexdigest()[:16]}.json"
            credential_path = self._auth_path / credential_name
            os.replace(candidates[0], credential_path)
            credential_path.chmod(0o600)
            records.append(
                {
                    "name": name,
                    "provider": "chatgpt",
                    "auth_mode": "subscription",
                    "credential_ref": credential_name,
                }
            )
            self._write_connections(records)
        return ProviderConnection(
            name=name,
            provider="chatgpt",
            auth_mode="subscription",
            status="connected",
        )

    def disconnect_connection(self, name: str) -> bool:
        """Remove one named upstream connection and revoke all of its routes."""
        name = _validate_connection_name(name)
        with self._locked():
            if not any(record["name"] == name for record in self._read_connections()):
                return False
        self.ensure_ready()
        with self._locked():
            records = self._read_connections()
            removed = [record for record in records if record["name"] == name]
            if not removed:
                return False
            remaining = [record for record in records if record["name"] != name]
            routes = self._read_routes()
            removed_routes = [route for route in routes if route["connection_name"] == name]
            remaining_routes = [route for route in routes if route["connection_name"] != name]
            self._replace_active_route_keys(remaining_routes)
            for route in removed_routes:
                self._await_route_status(route["api_key"], expected=401)
            self._write_routes(remaining_routes)
            self._write_connections(remaining)
            credential_ref = removed[0]["credential_ref"]
            if removed[0]["auth_mode"] == "api_key":
                (self._credentials_path / credential_ref).unlink(missing_ok=True)
            else:
                (self._auth_path / credential_ref).unlink(missing_ok=True)
        if removed[0]["auth_mode"] == "api_key":
            self.shutdown()
            self.ensure_ready()
        return True

    def create_route(
        self,
        connection_name: str,
        *,
        consumer: str,
        wire_api: str = "responses",
    ) -> InferenceRoute:
        """Issue one consumer-specific route backed by an existing connection."""
        if not consumer.strip():
            raise ValueError("consumer cannot be empty")
        if wire_api != "responses":
            raise ConsumerWireIncompatibleError(connection_name, wire_api)
        self.require_connection(connection_name)
        self.ensure_ready()
        with self._locked():
            connections = self._connections_unlocked()
            connection_names = {connection.name for connection in connections}
            if connection_name not in connection_names:
                raise ProviderConnectionNotFoundError(connection_name)
            prefixed = sum(connection.provider == "deepseek" for connection in connections)
            if prefixed > 1 or len(connections) - prefixed > 1:
                raise ProviderSelectionUnsupportedError(
                    "Multiple provider connections are not supported without distinct prefixes"
                )
            routes = self._read_routes()
            route = InferenceRoute(
                id=f"route-{os.urandom(8).hex()}",
                connection_name=connection_name,
                base_url=f"{self._origin}/v1",
                wire_api="responses",
                api_key=f"agw_{os.urandom(32).hex()}",
            )
            routes.append(
                {
                    "id": route.id,
                    "connection_name": route.connection_name,
                    "consumer": consumer.strip(),
                    "api_key": route.api_key,
                }
            )
            self._write_routes(routes)
            try:
                self._replace_active_route_keys(routes)
                self._await_route_status(route.api_key, expected=200)
            except ProviderGatewayError:
                routes = [item for item in routes if item["id"] != route.id]
                self._write_routes(routes)
                self._replace_active_route_keys(routes)
                raise
            return route

    def route_for_consumer(self, consumer: str) -> InferenceRoute | None:
        """Recover one durable consumer route without exposing other routes."""
        consumer = consumer.strip()
        if not consumer:
            raise ValueError("consumer cannot be empty")
        with self._locked():
            record = next(
                (route for route in self._read_routes() if route.get("consumer") == consumer),
                None,
            )
            if record is None:
                return None
            return InferenceRoute(
                id=record["id"],
                connection_name=record["connection_name"],
                base_url=f"{self._origin}/v1",
                wire_api="responses",
                api_key=record["api_key"],
            )

    def revoke_route(self, route_id: str) -> bool:
        """Revoke one route without affecting its upstream connection or siblings."""
        self.ensure_ready()
        with self._locked():
            routes = self._read_routes()
            removed = [route for route in routes if route.get("id") == route_id]
            if not removed:
                return False
            remaining = [route for route in routes if route.get("id") != route_id]
            self._write_routes(remaining)
            api_key = removed[0].get("api_key")
            try:
                self._replace_active_route_keys(remaining)
                if isinstance(api_key, str):
                    self._await_route_status(api_key, expected=401)
            except ProviderGatewayError:
                self._write_routes(routes)
                self._replace_active_route_keys(routes)
                raise
            return True

    def shutdown(self) -> None:
        """Stop the broker owned by this gateway state directory."""
        with self._locked():
            pid = self._read_pid()
            if pid is None:
                return
            if not self._process_matches(pid):
                self._pid_path.unlink(missing_ok=True)
                return
            self._terminate_process(pid)

    def _ready_health(self) -> GatewayHealth:
        return self._health("ready")

    @property
    def _origin(self) -> str:
        return f"http://{self._bind_address}:{self._port}"

    def _health(
        self,
        status: Literal["ready", "stopped", "unavailable"],
    ) -> GatewayHealth:
        return GatewayHealth(
            status=status,
            backend_present=self._executable_path.is_file(),
            backend_version=PINNED_BACKEND_VERSION,
            bind_addresses=(self._bind_address,),
            has_provider_connection=bool(self._connections_unlocked()),
        )

    def _verify_executable(self) -> None:
        try:
            result = subprocess.run(
                [self._executable_path, "-h"],
                text=True,
                capture_output=True,
                timeout=5,
                check=False,
            )
        except (OSError, subprocess.TimeoutExpired):
            raise GatewayUnavailableError("Provider gateway executable is unavailable") from None
        version_text = f"{result.stdout}\n{result.stderr}"
        if (
            result.returncode != 0
            or re.search(rf"(?<!\d){re.escape(PINNED_BACKEND_VERSION)}(?!\d)", version_text) is None
        ):
            raise GatewayUnavailableError("Provider gateway executable has an unsupported version")

    def _install_executable(self) -> None:
        try:
            asset_name, expected_checksum = _release_for_current_platform()
            archive_path = self._state_path / f".release-{os.getpid()}.tar.gz"
            digest = hashlib.sha256()
            request = urllib.request.Request(
                (
                    f"https://github.com/{_RELEASE_REPOSITORY}/releases/download/"
                    f"v{PINNED_BACKEND_VERSION}/{asset_name}"
                ),
                headers={"User-Agent": "dorf-provider-gateway"},
            )
            with urllib.request.urlopen(request, timeout=60) as response:
                descriptor = os.open(
                    archive_path,
                    os.O_WRONLY | os.O_CREAT | os.O_TRUNC,
                    0o600,
                )
                with os.fdopen(descriptor, "wb") as archive:
                    while chunk := response.read(1024 * 1024):
                        digest.update(chunk)
                        archive.write(chunk)
                    archive.flush()
                    os.fsync(archive.fileno())
            if digest.hexdigest() != expected_checksum:
                raise ValueError("release checksum mismatch")

            self._executable_path.parent.mkdir(parents=True, exist_ok=True, mode=0o700)
            self._executable_path.parent.chmod(0o700)
            temporary = self._executable_path.with_name(
                f".{self._executable_path.name}.{os.getpid()}.tmp"
            )
            with tarfile.open(archive_path, "r:gz") as release:
                member = release.getmember("cli-proxy-api")
                if not member.isfile():
                    raise ValueError("release executable is not a file")
                source = release.extractfile(member)
                if source is None:
                    raise ValueError("release executable is missing")
                descriptor = os.open(
                    temporary,
                    os.O_WRONLY | os.O_CREAT | os.O_TRUNC,
                    0o700,
                )
                with source, os.fdopen(descriptor, "wb") as target:
                    while chunk := source.read(1024 * 1024):
                        target.write(chunk)
                    target.flush()
                    os.fsync(target.fileno())
            os.replace(temporary, self._executable_path)
            self._executable_path.chmod(0o700)
        except (
            KeyError,
            OSError,
            tarfile.TarError,
            urllib.error.URLError,
            ValueError,
        ):
            raise GatewayUnavailableError(
                "Provider gateway executable could not be installed"
            ) from None
        finally:
            if "archive_path" in locals():
                archive_path.unlink(missing_ok=True)
            if "temporary" in locals():
                temporary.unlink(missing_ok=True)

    def _write_config(
        self,
        routes: list[dict[str, str]],
        authority: dict[str, str],
    ) -> None:
        self._auth_path.mkdir(parents=True, exist_ok=True, mode=0o700)
        self._auth_path.chmod(0o700)
        lines = [
            f"host: {json.dumps(self._bind_address)}",
            f"port: {self._port}",
            f"auth-dir: {json.dumps(str(self._auth_path))}",
            "force-model-prefix: true",
        ]
        # The concrete backend treats an empty list as authentication disabled. Keep one
        # unexposed key configured even when no consumer routes exist.
        keys = [authority["guard_key"]]
        keys.extend(
            route["api_key"]
            for route in routes
            if isinstance(route.get("api_key"), str) and route["api_key"]
        )
        lines.append("api-keys:")
        lines.extend(f"  - {json.dumps(key)}" for key in keys)
        lines.extend(
            (
                "remote-management:",
                (
                    "  allow-remote: false"
                    if self._bind_address == "127.0.0.1"
                    else "  allow-remote: true"
                ),
                f"  secret-key: {json.dumps(authority['management_key'])}",
                "  disable-control-panel: true",
                "debug: false",
                "logging-to-file: false",
                "usage-statistics-enabled: false",
                "",
            )
        )
        api_key_records = [
            record
            for record in self._read_connections()
            if record["provider"] in {"openai", "deepseek"} and record["auth_mode"] == "api_key"
        ]
        openai_records = [r for r in api_key_records if r["provider"] == "openai"]
        if openai_records:
            lines.append("codex-api-key:")
            for record in openai_records:
                secret = self._read_api_key(record["credential_ref"])
                lines.extend(
                    (
                        f"  - api-key: {json.dumps(secret)}",
                        '    base-url: "https://api.openai.com/v1"',
                    )
                )
            lines.append("")
        for record in (r for r in api_key_records if r["provider"] == "deepseek"):
            secret = self._read_api_key(record["credential_ref"])
            lines.extend(
                (
                    "openai-compatibility:",
                    '  - name: "deepseek"',
                    '    prefix: "deepseek"',
                    '    base-url: "https://api.deepseek.com/v1"',
                    "    api-key-entries:",
                    f"      - api-key: {json.dumps(secret)}",
                    "    models:",
                    '      - name: "deepseek-v4-flash"',
                    '        alias: "deepseek-v4-flash"',
                    "",
                )
            )
        config = "\n".join(lines)
        self._write_private_text(self._config_path, config)

    def _configured_bind_address(self) -> str | None:
        return _read_configured_bind_address(self._config_path)

    def _listener_configuration_matches(self) -> bool:
        try:
            config = self._config_path.read_text()
        except OSError:
            return False
        expected_remote = (
            "  allow-remote: false" if self._bind_address == "127.0.0.1" else "  allow-remote: true"
        )
        return (
            self._configured_bind_address() == self._bind_address
            and expected_remote in config.splitlines()
            and "  disable-control-panel: true" in config.splitlines()
        )

    def _connections_unlocked(self) -> tuple[ProviderConnection, ...]:
        records = self._read_connections()
        connections = []
        for record in records:
            credential_exists = self._credential_path(record).is_file()
            connections.append(
                ProviderConnection(
                    name=record["name"],
                    provider=record["provider"],
                    auth_mode=record["auth_mode"],
                    status=("connected" if credential_exists else "authentication_stale"),
                    remediation=(
                        None
                        if credential_exists
                        else _reconnect_remediation(
                            record["provider"],
                            record["auth_mode"],
                            record["name"],
                        )
                    ),
                )
            )
        if not self._auth_path.exists():
            return tuple(connections)
        self._auth_path.chmod(0o700)
        records_by_credential = {
            record["credential_ref"]: record
            for record in records
            if record["auth_mode"] == "subscription"
        }
        for path in sorted(self._auth_path.glob("*.json")):
            if path.is_symlink() or not path.is_file():
                continue
            if not path.name.startswith("codex-"):
                continue
            if path.name in records_by_credential:
                continue
            path.chmod(0o600)
            connection_name = f"connection-{hashlib.sha256(path.name.encode()).hexdigest()[:16]}"
            connections.append(
                ProviderConnection(
                    name=connection_name,
                    provider="chatgpt",
                    auth_mode="subscription",
                    status="connected",
                )
            )
        return tuple(connections)

    def _credential_path(self, record: dict[str, str]) -> Path:
        if record["auth_mode"] == "api_key":
            return self._credentials_path / record["credential_ref"]
        return self._auth_path / record["credential_ref"]

    def _snapshot_auth_files(self) -> dict[Path, int]:
        if not self._auth_path.exists():
            return {}
        snapshot: dict[Path, int] = {}
        for path in self._auth_path.glob("codex-*.json"):
            if path.is_symlink() or not path.is_file():
                continue
            try:
                snapshot[path] = path.stat().st_mtime_ns
            except OSError:
                continue
        return snapshot

    def _connection_backend_status(
        self,
        record: dict[str, str],
    ) -> dict[str, object] | None:
        authority = self._read_authority()
        request = urllib.request.Request(
            f"{self._origin}/v0/management/auth-files",
            headers={"Authorization": f"Bearer {authority['management_key']}"},
        )
        try:
            with urllib.request.urlopen(request, timeout=3) as response:
                raw = json.load(response)
        except (
            OSError,
            urllib.error.URLError,
            json.JSONDecodeError,
        ):
            raise GatewayUnavailableError(
                "Provider gateway connection status is unavailable"
            ) from None
        if not isinstance(raw, dict) or not isinstance(raw.get("files"), list):
            raise GatewayUnavailableError("Provider gateway connection status is unavailable")
        entries = [item for item in raw["files"] if isinstance(item, dict)]
        return next(
            (
                item
                for item in entries
                if item.get("name") == record["credential_ref"] and item.get("provider") == "codex"
            ),
            None,
        )

    def _read_connections(self) -> list[dict[str, str]]:
        try:
            raw = json.loads(self._connections_path.read_text())
        except FileNotFoundError:
            return []
        except (OSError, json.JSONDecodeError):
            raise GatewayUnavailableError(
                "Provider gateway connection state is unreadable"
            ) from None
        if not isinstance(raw, list):
            raise GatewayUnavailableError("Provider gateway connection state is unreadable")
        records: list[dict[str, str]] = []
        names: set[str] = set()
        credential_refs: set[str] = set()
        for item in raw:
            if not isinstance(item, dict):
                raise GatewayUnavailableError("Provider gateway connection state is unreadable")
            if not all(
                isinstance(item.get(field), str) and item[field]
                for field in ("name", "provider", "auth_mode", "credential_ref")
            ):
                raise GatewayUnavailableError("Provider gateway connection state is unreadable")
            name = item["name"]
            provider = item["provider"]
            auth_mode = item["auth_mode"]
            credential_ref = item["credential_ref"]
            try:
                _validate_connection_name(name)
            except ValueError:
                raise GatewayUnavailableError(
                    "Provider gateway connection state is unreadable"
                ) from None
            valid_credential = (
                provider == "chatgpt"
                and auth_mode == "subscription"
                and re.fullmatch(r"codex-dorf-[0-9a-f]{16}\.json", credential_ref) is not None
            ) or (
                provider in {"openai", "deepseek"}
                and auth_mode == "api_key"
                and re.fullmatch(rf"{provider}-[0-9a-f]{{16}}\.key", credential_ref) is not None
            )
            if not valid_credential or name in names or credential_ref in credential_refs:
                raise GatewayUnavailableError("Provider gateway connection state is unreadable")
            names.add(name)
            credential_refs.add(credential_ref)
            records.append(
                {
                    "name": name,
                    "provider": provider,
                    "auth_mode": auth_mode,
                    "credential_ref": credential_ref,
                }
            )
        return records

    def _write_connections(self, records: list[dict[str, str]]) -> None:
        self._write_private_text(
            self._connections_path,
            f"{json.dumps(records, indent=2, sort_keys=True)}\n",
        )

    def _read_api_key(self, credential_ref: str) -> str:
        try:
            value = (self._credentials_path / credential_ref).read_text().strip()
        except OSError:
            raise GatewayUnavailableError(
                "Provider connection authentication is unavailable"
            ) from None
        if not value:
            raise GatewayUnavailableError("Provider connection authentication is unavailable")
        return value

    def _read_routes(self) -> list[dict[str, str]]:
        try:
            raw = json.loads(self._routes_path.read_text())
        except FileNotFoundError:
            return []
        except (OSError, json.JSONDecodeError):
            raise GatewayUnavailableError("Provider gateway route state is unreadable") from None
        if not isinstance(raw, list) or any(not isinstance(item, dict) for item in raw):
            raise GatewayUnavailableError("Provider gateway route state is unreadable")
        routes = []
        for item in raw:
            if all(
                isinstance(item.get(field), str)
                for field in ("id", "connection_name", "consumer", "api_key")
            ):
                routes.append(
                    {
                        "id": item["id"],
                        "connection_name": item["connection_name"],
                        "consumer": item["consumer"],
                        "api_key": item["api_key"],
                    }
                )
        return routes

    def _write_routes(self, routes: list[dict[str, str]]) -> None:
        self._write_private_text(
            self._routes_path,
            f"{json.dumps(routes, indent=2, sort_keys=True)}\n",
        )

    def _read_authority(self) -> dict[str, str]:
        try:
            raw = json.loads(self._authority_path.read_text())
        except FileNotFoundError:
            raw = {
                "guard_key": f"agw_guard_{os.urandom(32).hex()}",
                "management_key": f"agw_control_{os.urandom(24).hex()}",
            }
            self._write_private_text(
                self._authority_path,
                f"{json.dumps(raw, indent=2, sort_keys=True)}\n",
            )
        except (OSError, json.JSONDecodeError):
            raise GatewayUnavailableError(
                "Provider gateway authority state is unreadable"
            ) from None
        if not isinstance(raw, dict) or not all(
            isinstance(raw.get(field), str) and raw[field]
            for field in ("guard_key", "management_key")
        ):
            raise GatewayUnavailableError("Provider gateway authority state is unreadable")
        return {
            "guard_key": raw["guard_key"],
            "management_key": raw["management_key"],
        }

    def _replace_active_route_keys(self, routes: list[dict[str, str]]) -> None:
        # Use the authenticated control endpoint: its file watcher does not observe the atomic
        # config replacements used for crash-safe gateway state.
        authority = self._read_authority()
        keys = [authority["guard_key"]]
        keys.extend(route["api_key"] for route in routes)
        request = urllib.request.Request(
            f"{self._origin}/v0/management/api-keys",
            data=json.dumps(keys).encode(),
            method="PUT",
            headers={
                "Authorization": f"Bearer {authority['management_key']}",
                "Content-Type": "application/json",
            },
        )
        try:
            with urllib.request.urlopen(request, timeout=3) as response:
                if response.status != 200:
                    raise GatewayUnavailableError("Provider gateway route change was rejected")
        except (OSError, urllib.error.URLError):
            raise GatewayUnavailableError("Provider gateway route change was rejected") from None

    def _await_route_status(self, api_key: str, *, expected: int) -> None:
        deadline = time.monotonic() + 3
        while time.monotonic() < deadline:
            status = self._route_status(api_key)
            if status == expected:
                return
            time.sleep(0.05)
        raise GatewayUnavailableError("Provider gateway route change did not become active")

    def _await_models_available(self, api_key: str) -> None:
        deadline = time.monotonic() + 10
        while time.monotonic() < deadline:
            request = urllib.request.Request(f"{self._origin}/v1/models")
            request.add_header("Authorization", f"Bearer {api_key}")
            try:
                with urllib.request.urlopen(request, timeout=0.5) as response:
                    payload = json.load(response)
            except (
                OSError,
                urllib.error.URLError,
                json.JSONDecodeError,
            ):
                payload = None
            if isinstance(payload, dict):
                models = payload.get("data")
                if isinstance(models, list) and models:
                    return
            time.sleep(0.05)
        raise GatewayUnavailableError("Provider gateway models did not become ready")

    def _route_status(self, api_key: str) -> int | None:
        request = urllib.request.Request(f"{self._origin}/v1/models")
        request.add_header("Authorization", f"Bearer {api_key}")
        try:
            with urllib.request.urlopen(request, timeout=0.5) as response:
                return response.status
        except urllib.error.HTTPError as error:
            return error.code
        except (OSError, urllib.error.URLError):
            return None

    def _start_process(self) -> subprocess.Popen[bytes]:
        descriptor = os.open(self._log_path, os.O_WRONLY | os.O_CREAT | os.O_APPEND, 0o600)
        try:
            return subprocess.Popen(
                [
                    self._executable_path,
                    "-config",
                    self._config_path,
                    "-local-model",
                ],
                cwd=self._state_path,
                stdin=subprocess.DEVNULL,
                stdout=descriptor,
                stderr=subprocess.STDOUT,
                start_new_session=True,
            )
        except OSError:
            raise GatewayUnavailableError("Provider gateway failed to start") from None
        finally:
            os.close(descriptor)

    def _terminate_process(self, pid: int) -> None:
        try:
            os.killpg(pid, signal.SIGTERM)
        except ProcessLookupError:
            pass
        deadline = time.monotonic() + 3
        while self._process_alive(pid) and time.monotonic() < deadline:
            self._reap_process(pid)
            time.sleep(0.05)
        self._reap_process(pid)
        self._pid_path.unlink(missing_ok=True)

    def _probe_ready(self) -> bool:
        try:
            with urllib.request.urlopen(
                f"{self._origin}/",
                timeout=0.5,
            ) as response:
                return response.status == 200
        except (OSError, urllib.error.URLError):
            return False

    def _read_pid(self) -> int | None:
        try:
            raw = self._pid_path.read_text().strip()
            pid = int(raw)
        except (FileNotFoundError, ValueError, OSError):
            return None
        return pid if pid > 1 else None

    def _process_matches(self, pid: int) -> bool:
        if not self._process_alive(pid):
            return False
        try:
            argv = (Path("/proc") / str(pid) / "cmdline").read_bytes().split(b"\0")
        except OSError:
            return False
        expected_executable = os.fsencode(self._executable_path)
        expected_config = os.fsencode(self._config_path)
        return expected_executable in argv and expected_config in argv

    @staticmethod
    def _process_alive(pid: int) -> bool:
        try:
            os.kill(pid, 0)
        except ProcessLookupError:
            return False
        except PermissionError:
            return True
        return True

    @staticmethod
    def _reap_process(pid: int) -> None:
        try:
            os.waitpid(pid, os.WNOHANG)
        except ChildProcessError:
            pass

    def _locked(self):
        self._state_path.mkdir(parents=True, exist_ok=True, mode=0o700)
        self._state_path.chmod(0o700)
        self._lock_path.touch(mode=0o600, exist_ok=True)
        self._lock_path.chmod(0o600)
        descriptor = self._lock_path.open("r+")

        class Lock:
            def __enter__(inner_self):
                fcntl.flock(descriptor, fcntl.LOCK_EX)
                return inner_self

            def __exit__(inner_self, *exc_info: object) -> None:
                fcntl.flock(descriptor, fcntl.LOCK_UN)
                descriptor.close()

        return Lock()

    @staticmethod
    def _write_private_text(path: Path, content: str) -> None:
        temporary = path.with_name(f".{path.name}.{os.getpid()}.tmp")
        descriptor = os.open(temporary, os.O_WRONLY | os.O_CREAT | os.O_TRUNC, 0o600)
        try:
            with os.fdopen(descriptor, "w") as stream:
                stream.write(content)
                stream.flush()
                os.fsync(stream.fileno())
            os.replace(temporary, path)
            path.chmod(0o600)
        finally:
            temporary.unlink(missing_ok=True)


__all__ = [
    "ConsumerWireIncompatibleError",
    "GatewayHealth",
    "GatewayUnavailableError",
    "DeviceAuthorization",
    "InferenceRoute",
    "ProviderAuthenticationError",
    "ProviderAuthenticationStaleError",
    "ProviderUpstreamUnavailableError",
    "ProviderConnection",
    "ProviderConnectionNotFoundError",
    "ProviderGateway",
    "ProviderGatewayError",
    "ProviderSelectionUnsupportedError",
]


def _read_configured_bind_address(config_path: Path) -> str | None:
    try:
        config = config_path.read_text()
    except OSError:
        return None
    match = re.search(r'^host:\s*"([^"]+)"\s*$', config, re.MULTILINE)
    return match.group(1) if match is not None else None


def _default_state_path() -> Path:
    data_home = os.environ.get("XDG_DATA_HOME")
    if data_home:
        return Path(data_home) / "dorf" / "provider-gateway"
    return Path.home() / ".local" / "share" / "dorf" / "provider-gateway"


def _release_for_current_platform() -> tuple[str, str]:
    if platform.system() != "Linux":
        raise ValueError("unsupported provider gateway platform")
    machine = platform.machine().lower()
    if machine == "arm64":
        machine = "aarch64"
    try:
        return _LINUX_RELEASES[machine]
    except KeyError as error:
        raise ValueError("unsupported provider gateway architecture") from error


def _validate_connection_name(name: str) -> str:
    name = name.strip()
    if re.fullmatch(r"[a-z][a-z0-9]*(?:-[a-z0-9]+)*", name) is None:
        raise ValueError("connection name must use lowercase letters, numbers, and single hyphens")
    return name


def _reconnect_remediation(provider: str, auth_mode: str, name: str) -> str:
    if provider == "chatgpt" and auth_mode == "subscription":
        return f"Run: dorf provider connect chatgpt --subscription --name {name}"
    return f"Run: dorf provider connect {provider} --api-key --name {name}"


def _safe_plan(entry: dict[str, object]) -> str | None:
    id_token = entry.get("id_token")
    if not isinstance(id_token, dict):
        return None
    value = id_token.get("plan_type")
    if not isinstance(value, str):
        return None
    value = value.strip()
    return value if re.fullmatch(r"[A-Za-z0-9_-]{1,32}", value) else None
