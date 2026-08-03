import subprocess
from dataclasses import replace
from pathlib import Path

import pytest
from test_incus import FakeProbe, worker_binding

from dorf.adapters.environments import IncusEnvironment
from dorf.codex_room import (
    CODEX_ROUTE_CREDENTIAL_PATH,
    PROVIDER_CONNECTION_METADATA_KEY,
    CodexRoomEnvironment,
    recorded_codex_room_environment,
)
from dorf.provider_gateway import (
    InferenceRoute,
    ProviderAuthenticationStaleError,
    ProviderConnection,
)
from dorf.runtime import NewWorker, RuntimeStore, WorkerRuntime


class RecordingGateway:
    def __init__(self) -> None:
        self.created: list[tuple[str, str]] = []
        self.revoked: list[str] = []
        self.routes: dict[str, InferenceRoute] = {}
        self.revoke_failure: RuntimeError | None = None
        self.required: list[str] = []

    def require_connection(self, connection_name: str) -> ProviderConnection:
        self.required.append(connection_name)
        return ProviderConnection(
            connection_name,
            "chatgpt",
            "subscription",
            "connected",
        )

    def create_route(
        self,
        connection_name: str,
        *,
        consumer: str,
        wire_api: str = "responses",
    ) -> InferenceRoute:
        self.created.append((connection_name, consumer))
        route = InferenceRoute(
            id=f"route-{len(self.created)}",
            connection_name=connection_name,
            base_url="http://10.42.0.1:8317/v1",
            wire_api="responses",
            api_key="room-scoped-key",
        )
        self.routes[consumer] = route
        return route

    def route_for_consumer(self, consumer: str) -> InferenceRoute | None:
        return self.routes.get(consumer)

    def revoke_route(self, route_id: str) -> bool:
        self.revoked.append(route_id)
        if self.revoke_failure is not None:
            raise self.revoke_failure
        consumer = next(
            (consumer for consumer, route in self.routes.items() if route.id == route_id),
            None,
        )
        if consumer is None:
            return False
        del self.routes[consumer]
        return True


class ReadyAgent:
    agent_type = "codex"

    def prepare(self, binding) -> None:
        pass


class FailingAgent(ReadyAgent):
    def prepare(self, binding) -> None:
        raise RuntimeError("route readiness failed")


def test_room_prerequisites_fail_on_stale_auth_before_vm_provisioning() -> None:
    class StaleGateway(RecordingGateway):
        def require_connection(self, connection_name: str) -> ProviderConnection:
            raise ProviderAuthenticationStaleError(
                connection_name,
                provider="chatgpt",
                auth_mode="subscription",
            )

    probe = FakeProbe()
    environment = CodexRoomEnvironment(
        IncusEnvironment(probe=probe, sleep=lambda seconds: None),
        StaleGateway(),
        connection_name="personal-chatgpt",
    )

    with pytest.raises(ProviderAuthenticationStaleError) as caught:
        environment.check_prerequisites()

    assert caught.value.needs_human is True
    assert not any(command[:2] == ["incus", "init"] for command in probe.ran)


def test_recorded_room_reconstructs_the_routed_codex_facade() -> None:
    gateway = RecordingGateway()
    binding = worker_binding()
    binding = replace(
        binding,
        room=replace(
            binding.room,
            metadata={
                "template": "dorf-codex",
                PROVIDER_CONNECTION_METADATA_KEY: "personal-chatgpt",
            },
        ),
    )

    environment = recorded_codex_room_environment(binding, gateway=gateway)

    assert isinstance(environment, CodexRoomEnvironment)
    assert environment.config.template == "dorf-codex"
    assert (
        environment.initial_metadata(binding.worker.name)[PROVIDER_CONNECTION_METADATA_KEY]
        == "personal-chatgpt"
    )


def test_recorded_room_without_provider_connection_is_rejected() -> None:
    with pytest.raises(RuntimeError, match="has no Provider Connection"):
        recorded_codex_room_environment(worker_binding(), gateway=RecordingGateway())


def test_codex_room_installs_only_derived_config_and_route_key_then_revokes() -> None:
    probe = FakeProbe()
    gateway = RecordingGateway()
    environment = CodexRoomEnvironment(
        IncusEnvironment(probe=probe, sleep=lambda seconds: None),
        gateway,
        connection_name="personal-chatgpt",
    )
    binding = worker_binding()

    environment.create(binding)

    assert gateway.created == [("personal-chatgpt", "room:room-1")]
    writes = [
        (command, input_value)
        for command, input_value in zip(probe.ran, probe.inputs, strict=True)
        if input_value is not None
    ]
    assert len(writes) == 2
    config_command, config = writes[0]
    credential_command, credential = writes[1]
    assert ["bash", "-lc"] == config_command[-3:-1]
    assert "/root/.codex/config.toml" in config_command[-1]
    assert config == (
        'model_provider = "dorf"\n'
        "\n"
        "[model_providers.dorf]\n"
        'name = "Dorf Provider Gateway"\n'
        'base_url = "http://10.42.0.1:8317/v1"\n'
        'env_key = "DORF_PROVIDER_ROUTE_KEY"\n'
        'wire_api = "responses"\n'
        "requires_openai_auth = false\n"
    )
    assert CODEX_ROUTE_CREDENTIAL_PATH in credential_command[-1]
    assert credential == "room-scoped-key\n"
    assert "room-scoped-key" not in repr(probe.ran)

    command = environment.process_command(
        binding,
        ["codex", "app-server", "--listen", "ws://10.42.0.19:4500"],
        cwd="/workspace",
        env={"DORF_WORKSPACE": "/workspace"},
    )
    assert "room-scoped-key" not in repr(command)
    assert CODEX_ROUTE_CREDENTIAL_PATH in " ".join(command)
    assert command[-3:] == [
        "app-server",
        "--listen",
        "ws://10.42.0.19:4500",
    ]

    environment.check_codex_authentication(binding)
    route_probe = probe.ran[-1]
    assert "curl" in " ".join(route_probe)
    assert CODEX_ROUTE_CREDENTIAL_PATH in " ".join(route_probe)
    assert "room-scoped-key" not in repr(route_probe)

    assert environment.destroy(binding) == "deleted"
    assert gateway.revoked == ["route-1"]
    assert probe.ran[-1] == [
        "incus",
        "delete",
        "dorf-coder-demo",
        "--force",
    ]


def test_room_end_remains_retryable_until_route_revocation_succeeds(
    tmp_path: Path,
) -> None:
    probe = FakeProbe()
    gateway = RecordingGateway()
    environment = CodexRoomEnvironment(
        IncusEnvironment(probe=probe, sleep=lambda seconds: None),
        gateway,
        connection_name="personal-chatgpt",
    )
    store = RuntimeStore.open(tmp_path / "state.sqlite3")
    runtime = WorkerRuntime(store, environment, ReadyAgent())
    binding = runtime.spawn(NewWorker("researcher"))
    gateway.revoke_failure = RuntimeError("route authority unavailable")

    with pytest.raises(RuntimeError, match="route authority unavailable"):
        runtime.end("researcher")

    assert store.get_room(binding.room.id).status == "cleanup-failed"
    assert store.get_worker("researcher").status == "ending"
    assert gateway.route_for_consumer(f"room:{binding.room.id}") is not None

    gateway.revoke_failure = None
    ended = runtime.end("researcher")

    assert ended.status == "ended"
    assert gateway.route_for_consumer(f"room:{binding.room.id}") is None


def test_failure_after_route_creation_destroys_partial_room_and_revokes_route(
    tmp_path: Path,
) -> None:
    class CredentialWriteFailure(FakeProbe):
        def run(self, argv, *, input=None, timeout_seconds=None):
            result = super().run(
                argv,
                input=input,
                timeout_seconds=timeout_seconds,
            )
            if input == "room-scoped-key\n":
                return subprocess.CompletedProcess(argv, 1, "", "write failed")
            return result

    probe = CredentialWriteFailure()
    gateway = RecordingGateway()
    environment = CodexRoomEnvironment(
        IncusEnvironment(probe=probe, sleep=lambda seconds: None),
        gateway,
        connection_name="personal-chatgpt",
    )
    store = RuntimeStore.open(tmp_path / "state.sqlite3")
    runtime = WorkerRuntime(store, environment, ReadyAgent())

    with pytest.raises(RuntimeError, match="route credential"):
        runtime.spawn(NewWorker("researcher"))

    room = store.get_current_room("researcher")
    assert room is not None
    assert [
        "incus",
        "delete",
        "dorf-researcher",
        "--force",
    ] in probe.ran
    assert gateway.route_for_consumer(f"room:{room.id}") is None


def test_agent_readiness_failure_also_destroys_room_and_revokes_route(
    tmp_path: Path,
) -> None:
    probe = FakeProbe()
    gateway = RecordingGateway()
    environment = CodexRoomEnvironment(
        IncusEnvironment(probe=probe, sleep=lambda seconds: None),
        gateway,
        connection_name="personal-chatgpt",
    )
    store = RuntimeStore.open(tmp_path / "state.sqlite3")
    runtime = WorkerRuntime(store, environment, FailingAgent())

    with pytest.raises(RuntimeError, match="route readiness failed"):
        runtime.spawn(NewWorker("researcher"))

    room = store.get_current_room("researcher")
    assert room is not None
    assert room.status == "failed"
    assert [
        "incus",
        "delete",
        "dorf-researcher",
        "--force",
    ] in probe.ran
    assert gateway.route_for_consumer(f"room:{room.id}") is None
