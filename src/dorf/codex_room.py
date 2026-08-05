"""Concrete composition of one Codex Room with one scoped inference route."""

from __future__ import annotations

import json
import shlex
import subprocess
from collections.abc import Mapping
from typing import Protocol

from dorf.adapters.environments import (
    IncusConfig,
    IncusEnvironment,
    IncusRunnerProbe,
    incus_bridge_ipv4,
)
from dorf.provider_gateway import (
    InferenceRoute,
    ProviderConnection,
    ProviderGateway,
)
from dorf.runtime import JobBinding, WorkerBinding

CODEX_CONFIG_PATH = "/root/.codex/config.toml"
CODEX_ROUTE_CREDENTIAL_PATH = "/root/.config/dorf/provider-route.key"
CODEX_ROUTE_ENV_KEY = "DORF_PROVIDER_ROUTE_KEY"
PROVIDER_CONNECTION_METADATA_KEY = "provider_connection"


class RoomRouteGateway(Protocol):
    def require_connection(self, name: str) -> ProviderConnection: ...

    def create_route(
        self,
        connection_name: str,
        *,
        consumer: str,
        wire_api: str = "responses",
    ) -> InferenceRoute: ...

    def route_for_consumer(self, consumer: str) -> InferenceRoute | None: ...

    def revoke_route(self, route_id: str) -> bool: ...


class CodexRoomEnvironment:
    """Install and retire the scoped model-plane capability for one Incus Room."""

    environment_type = IncusEnvironment.environment_type
    workspace = IncusEnvironment.workspace

    def __init__(
        self,
        environment: IncusEnvironment,
        gateway: RoomRouteGateway,
        *,
        connection_name: str,
    ) -> None:
        self._environment = environment
        self._gateway = gateway
        self._connection_name = connection_name
        self.config = environment.config

    def check_prerequisites(self) -> list[str]:
        self._gateway.require_connection(self._connection_name)
        return self._environment.check_prerequisites()

    def environment_id(self, worker_name: str) -> str:
        return self._environment.environment_id(worker_name)

    def initial_metadata(self, worker_name: str) -> dict[str, str]:
        return {
            **self._environment.initial_metadata(worker_name),
            PROVIDER_CONNECTION_METADATA_KEY: self._connection_name,
        }

    def create(self, binding: WorkerBinding) -> None:
        self._environment.create(binding)
        self.install_provider_route(binding)

    def install_provider_route(self, binding: WorkerBinding | JobBinding) -> None:
        consumer = _room_consumer(binding)
        route = self._gateway.route_for_consumer(consumer)
        if route is None:
            route = self._gateway.create_route(
                self._connection_name,
                consumer=consumer,
            )
        self._install_route(binding, route)

    def restore(self, binding: WorkerBinding) -> str:
        return self._environment.restore(binding)

    def stop(self, binding: WorkerBinding) -> str:
        return self._environment.stop(binding)

    def destroy(self, binding: WorkerBinding) -> str:
        outcome = self._environment.destroy(binding)
        route = self._gateway.route_for_consumer(_room_consumer(binding))
        if route is not None:
            self._gateway.revoke_route(route.id)
        return outcome

    def execute(
        self,
        binding: WorkerBinding | JobBinding,
        argv: list[str],
        *,
        cwd: str | None = None,
        env: Mapping[str, str] | None = None,
        input: str | None = None,
        timeout_seconds: float | None = None,
        provider_route: bool = False,
    ) -> subprocess.CompletedProcess[str]:
        return self._environment.execute(
            binding,
            self._provider_route_argv(argv) if provider_route else self._room_argv(argv),
            cwd=cwd,
            env=env,
            input=input,
            timeout_seconds=timeout_seconds,
        )

    def process_command(
        self,
        binding: WorkerBinding | JobBinding,
        argv: list[str],
        *,
        cwd: str | None = None,
        env: Mapping[str, str] | None = None,
        provider_route: bool = False,
    ) -> list[str]:
        return self._environment.process_command(
            binding,
            self._provider_route_argv(argv) if provider_route else self._room_argv(argv),
            cwd=cwd,
            env=env,
        )

    def attach(self, binding: WorkerBinding, *, cwd: str) -> int:
        return self._environment.attach(binding, cwd=cwd)

    def check_codex_authentication(
        self,
        binding: WorkerBinding | JobBinding,
    ) -> None:
        route = self._gateway.route_for_consumer(_room_consumer(binding))
        if route is None:
            raise RuntimeError("Codex inference route is unavailable")
        models_url = shlex.quote(f"{route.base_url}/models")
        script = (
            f"IFS= read -r {CODEX_ROUTE_ENV_KEY} < {CODEX_ROUTE_CREDENTIAL_PATH}; "
            f"curl --fail --silent --show-error --output /dev/null "
            f'-H "Authorization: Bearer ${CODEX_ROUTE_ENV_KEY}" {models_url}'
        )
        result = self._environment.execute(binding, ["bash", "-lc", script])
        if result.returncode != 0:
            raise RuntimeError("Codex inference route authentication is unavailable")

    def pull_file(
        self,
        binding: WorkerBinding | JobBinding,
        room_path: str,
        destination,
        *,
        max_bytes: int,
    ) -> None:
        self._environment.pull_file(
            binding,
            room_path,
            destination,
            max_bytes=max_bytes,
        )

    def _install_route(
        self,
        binding: WorkerBinding | JobBinding,
        route: InferenceRoute,
    ) -> None:
        config = _codex_config(route)
        self._write_private_room_file(
            binding,
            path=CODEX_CONFIG_PATH,
            directory="/root/.codex",
            content=config,
            label="Codex provider configuration",
        )
        self._write_private_room_file(
            binding,
            path=CODEX_ROUTE_CREDENTIAL_PATH,
            directory="/root/.config/dorf",
            content=f"{route.api_key}\n",
            label="Codex inference route credential",
        )

    def _write_private_room_file(
        self,
        binding: WorkerBinding,
        *,
        path: str,
        directory: str,
        content: str,
        label: str,
    ) -> None:
        result = self._environment.execute(
            binding,
            [
                "bash",
                "-lc",
                f"umask 077; mkdir -p {directory}; cat > {path}",
            ],
            input=content,
        )
        if result.returncode != 0:
            raise RuntimeError(f"Could not install {label}")

    @staticmethod
    def _room_argv(argv: list[str]) -> list[str]:
        if not argv or argv[0] != "codex":
            return argv
        script = (
            f"IFS= read -r {CODEX_ROUTE_ENV_KEY} < {CODEX_ROUTE_CREDENTIAL_PATH}; "
            f'export {CODEX_ROUTE_ENV_KEY}; exec codex "$@"'
        )
        return ["bash", "-lc", script, "dorf-codex", *argv[1:]]

    @staticmethod
    def _provider_route_argv(argv: list[str]) -> list[str]:
        script = (
            f"IFS= read -r {CODEX_ROUTE_ENV_KEY} < {CODEX_ROUTE_CREDENTIAL_PATH}; "
            f"export {CODEX_ROUTE_ENV_KEY}; exec \"$@\""
        )
        return ["bash", "-lc", script, "dorf-provider-route", *argv]


def new_codex_room_environment(
    config: IncusConfig,
    connection_name: str,
    *,
    gateway: RoomRouteGateway | None = None,
    probe: IncusRunnerProbe | None = None,
) -> CodexRoomEnvironment:
    """Compose the concrete credential-free Codex Room boundary."""
    gateway = gateway or ProviderGateway.open(bind_address=incus_bridge_ipv4(config.network))
    return CodexRoomEnvironment(
        IncusEnvironment(config, probe=probe),
        gateway,
        connection_name=connection_name,
    )


def recorded_codex_room_environment(
    binding: WorkerBinding | JobBinding,
    *,
    gateway: RoomRouteGateway | None = None,
    probe: IncusRunnerProbe | None = None,
) -> CodexRoomEnvironment:
    """Reconstruct the same Room boundary in detached controller processes."""
    if binding.environment_type != IncusEnvironment.environment_type:
        raise RuntimeError(f"Unsupported Room type: {binding.environment_type}")
    connection_name = binding.metadata.get(PROVIDER_CONNECTION_METADATA_KEY)
    if connection_name is None:
        raise RuntimeError(
            "Recorded Room has no Provider Connection; recreate it with the current "
            "credential-free Room contract"
        )
    return new_codex_room_environment(
        IncusConfig.from_mapping(binding.metadata),
        connection_name,
        gateway=gateway,
        probe=probe,
    )


def _room_consumer(binding: WorkerBinding | JobBinding) -> str:
    return f"room:{binding.room.id}"


def _codex_config(route: InferenceRoute) -> str:
    return "\n".join(
        (
            'model_provider = "dorf"',
            "",
            "[model_providers.dorf]",
            'name = "Dorf Provider Gateway"',
            f"base_url = {json.dumps(route.base_url)}",
            f"env_key = {json.dumps(CODEX_ROUTE_ENV_KEY)}",
            f"wire_api = {json.dumps(route.wire_api)}",
            "requires_openai_auth = false",
            "",
        )
    )
