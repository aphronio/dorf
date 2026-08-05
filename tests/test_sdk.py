import sqlite3
import subprocess
from dataclasses import asdict

import pytest

from dorf import Dorf
from dorf.adapters.agents.codex_config import CodexConfig
from dorf.adapters.environments import IncusConfig
from dorf.provider_gateway import (
    InferenceRoute,
    ProviderConnection,
    ProviderGateway,
)
from dorf.runtime import AgentTurnRecovery, ArtifactInput, RuntimeStore


def configure_passing_incus(monkeypatch) -> list[list[str]]:
    monkeypatch.setattr(
        "dorf.adapters.environments.incus.IncusDoctor.fast_check",
        lambda self, config: type("Result", (), {"ok": True, "failures": []})(),
    )
    commands: list[list[str]] = []
    routes: dict[str, InferenceRoute] = {}

    def run(self, argv, *, input=None, timeout_seconds=None):
        commands.append(argv)
        if argv[:3] == ["incus", "network", "get"] and argv[-1] == "ipv4.address":
            return subprocess.CompletedProcess(argv, 0, "10.42.0.1/24\n", "")
        if "--" in argv:
            tail = argv[argv.index("--") + 1 :]
            if tail == ["ip", "-4", "route", "get", "1.1.1.1"]:
                return subprocess.CompletedProcess(
                    argv,
                    0,
                    "1.1.1.1 via 10.42.0.1 dev enp5s0 src 10.42.0.19 uid 0\n",
                    "",
                )
        return subprocess.CompletedProcess(argv, 0, "", "")

    monkeypatch.setattr("dorf.adapters.environments.incus.IncusRunnerProbe.run", run)
    monkeypatch.setattr(
        ProviderGateway,
        "require_connection",
        lambda self, connection_name: ProviderConnection(
            connection_name,
            "chatgpt",
            "subscription",
            "connected",
        ),
    )

    def create_route(self, connection_name, *, consumer, wire_api="responses"):
        route = InferenceRoute(
            f"route-{len(routes) + 1}",
            connection_name,
            "http://10.42.0.1:8317/v1",
            "responses",
            "room-key",
        )
        routes[consumer] = route
        return route

    monkeypatch.setattr(ProviderGateway, "create_route", create_route)
    monkeypatch.setattr(
        ProviderGateway,
        "route_for_consumer",
        lambda self, consumer: routes.get(consumer),
    )

    def revoke_route(self, route_id):
        consumer = next(
            (name for name, route in routes.items() if route.id == route_id),
            None,
        )
        if consumer is None:
            return False
        del routes[consumer]
        return True

    monkeypatch.setattr(ProviderGateway, "revoke_route", revoke_route)
    return commands


def test_sdk_context_manager_closes_its_store(tmp_path) -> None:
    with Dorf.open(tmp_path / "state.sqlite3") as dorf:
        assert dorf.get_worker("missing") is None

    with pytest.raises(sqlite3.ProgrammingError, match="closed database"):
        dorf.get_worker("missing")


def test_sdk_requires_explicit_provider_connection_for_a_new_room(tmp_path) -> None:
    dorf = Dorf.open(tmp_path / "state.sqlite3")

    with pytest.raises(ValueError, match="Provider Connection"):
        dorf.spawn_worker("researcher")


def test_sdk_composes_one_explicit_provider_route_with_room_lifecycle(
    tmp_path,
    monkeypatch,
) -> None:
    configure_passing_incus(monkeypatch)

    class Gateway:
        def __init__(self) -> None:
            self.route = None
            self.revoked = []

        def require_connection(self, connection_name):
            return ProviderConnection(
                connection_name,
                "chatgpt",
                "subscription",
                "connected",
            )

        def create_route(self, connection_name, *, consumer, wire_api="responses"):
            self.route = InferenceRoute(
                "route-sdk",
                connection_name,
                "http://10.42.0.1:8317/v1",
                "responses",
                "room-key",
            )
            self.consumer = consumer
            return self.route

        def route_for_consumer(self, consumer):
            return self.route if consumer == getattr(self, "consumer", None) else None

        def revoke_route(self, route_id):
            self.revoked.append(route_id)
            self.route = None
            return True

        def close(self):
            pass

    gateway = Gateway()
    dorf = Dorf.open(
        tmp_path / "state.sqlite3",
        provider_connection="personal-chatgpt",
        provider_gateway=gateway,
    )

    binding = dorf.spawn_worker("researcher")

    assert binding.room.metadata["provider_connection"] == "personal-chatgpt"
    assert gateway.consumer == f"room:{binding.room.id}"

    ended = dorf.end_worker("researcher")

    assert ended.worker.status == "ended"
    assert gateway.revoked == ["route-sdk"]


def test_sdk_composes_resource_lifecycle_and_retry_safe_messages(tmp_path, monkeypatch) -> None:
    configure_passing_incus(monkeypatch)
    launched: list[tuple[str, str]] = []
    monkeypatch.setattr(
        "dorf.sdk.launch_worker_message_dispatcher",
        lambda database, name: launched.append(("worker", name)) or False,
    )
    monkeypatch.setattr(
        "dorf.sdk.launch_job_input_dispatcher",
        lambda database, name: launched.append(("job", name)) or False,
    )
    monkeypatch.setattr(
        "dorf.sdk.launch_assignment_report_collector",
        lambda database, job, assignment: launched.append(("collector", job)) or True,
    )
    dorf = Dorf.open(
        tmp_path / "state.sqlite3",
        environment_config=IncusConfig(
            template="test-template",
            network="test-network",
            root_disk_size="8GiB",
        ),
        agent_defaults=CodexConfig("test-model", "medium"),
        provider_connection="personal-chatgpt",
    )

    worker = dorf.spawn_worker("researcher")
    first = dorf.message_worker(
        "researcher",
        "Investigate the failure",
        action_id="client-worker-action-1",
    )
    retried = dorf.message_worker(
        "researcher",
        "Investigate the failure",
        action_id="client-worker-action-1",
    )
    with pytest.raises(ValueError, match="already bound to different input"):
        dorf.message_worker(
            "researcher",
            "Use the same action for different text",
            action_id="client-worker-action-1",
        )
    assignment = dorf.assign_job(
        "checkout-perf",
        worker_name="researcher",
        goal="Make checkout instant",
    )
    followup = dorf.message_job(
        "checkout-perf",
        "Profile the API first",
        action_id="run_123:tool_call_abc_def",
    )
    followup_retry = dorf.message_job(
        "checkout-perf",
        "Profile the API first",
        action_id="run_123:tool_call_abc_def",
    )

    assert worker.room.metadata == {
        "template": "test-template",
        "network": "test-network",
        "root_disk_size": "8GiB",
        "provider_connection": "personal-chatgpt",
    }
    assert first.created is True
    assert retried.created is False
    assert retried.message == first.message
    assert assignment.binding.job.goal == "Make checkout instant"
    assert assignment.initial_input.sequence == 1
    assert assignment.initial_input.kind == "goal"
    assert followup.created is True
    assert followup_retry.created is False
    assert followup_retry.job_input == followup.job_input
    assert followup.job_input.id.startswith("jmsg-")
    assert ":" not in followup.job_input.id
    assert "_" not in followup.job_input.id
    admitted_events = [
        event
        for event in dorf.job_timeline("checkout-perf")
        if event.kind == "input-admitted" and event.related.get("input") == followup.job_input.id
    ]
    assert len(admitted_events) == 1
    assert dorf.inspect_worker("researcher").current_job_name == "checkout-perf"
    assert dorf.inspect_job("checkout-perf").queued_inputs == 2
    assert dorf.wait_for_worker_message("researcher", timeout=0).outcome == "working"
    assert dorf.wait_for_job_input("checkout-perf", timeout=0).outcome == "working"
    assert launched == [
        ("worker", "researcher"),
        ("worker", "researcher"),
        ("collector", "checkout-perf"),
        ("job", "checkout-perf"),
        ("collector", "checkout-perf"),
        ("job", "checkout-perf"),
        ("collector", "checkout-perf"),
        ("job", "checkout-perf"),
    ]


def test_public_job_message_is_blocked_while_assignment_is_preparing(
    tmp_path, monkeypatch
) -> None:
    configure_passing_incus(monkeypatch)
    launched = []
    monkeypatch.setattr(
        "dorf.sdk.launch_job_input_dispatcher",
        lambda database, name: launched.append(("dispatcher", name)) or True,
    )
    monkeypatch.setattr(
        "dorf.sdk.launch_assignment_report_collector",
        lambda database, job, assignment: launched.append(("collector", job)) or True,
    )
    database = tmp_path / "state.sqlite3"
    dorf = Dorf.open(
        database,
        agent_defaults=CodexConfig("test-model", "medium"),
        provider_connection="personal-chatgpt",
    )
    dorf.spawn_worker("coder")
    dorf.assign_job("setup-race", worker_name="coder", goal="Implement after preparation")
    RuntimeStore.open(database).update_assignment_status("setup-race", "preparing")
    launched.clear()

    with pytest.raises(RuntimeError, match="Job is not open: setup-race"):
        dorf.message_job(
            "setup-race",
            "Concurrent public message",
            action_id="concurrent-message",
        )

    assert len(RuntimeStore.open(database).list_job_inputs("setup-race")) == 1
    assert launched == []

    RuntimeStore.open(database).update_assignment_status("setup-race", "open")
    admitted = dorf.message_job(
        "setup-race",
        "Concurrent public message",
        action_id="concurrent-message",
    )

    assert admitted.created is True
    assert admitted.job_input.sequence == 2
    assert admitted.dispatcher_started is True
    assert launched == [("collector", "setup-race"), ("dispatcher", "setup-race")]


def test_sdk_reads_retained_artifact_after_job_and_room_cleanup(
    tmp_path,
    monkeypatch,
) -> None:
    configure_passing_incus(monkeypatch)
    monkeypatch.setattr(
        "dorf.sdk.launch_job_input_dispatcher",
        lambda database, name: False,
    )
    monkeypatch.setattr(
        "dorf.sdk.launch_assignment_report_collector",
        lambda database, job, assignment: False,
    )
    dorf = Dorf.open(
        tmp_path / "state.sqlite3",
        provider_connection="personal-chatgpt",
    )
    dorf.spawn_worker("researcher")
    assignment = dorf.assign_job(
        "checkout-perf",
        worker_name="researcher",
        goal="Identify the checkout bottleneck",
    )
    source = tmp_path / "finding.json"
    source.write_text('{"bottleneck":"cart totals","p95_ms":120}\n')
    dorf._store.documents.append_event(
        "checkout-perf",
        event_id="report-finding",
        source="worker",
        provenance="claim",
        kind="completion",
        summary="Reported the checkout bottleneck",
        related={"assignment": assignment.binding.assignment.id},
        artifacts=[ArtifactInput("finding.json", source, "application/json")],
    )

    dorf.end_job("checkout-perf", interrupt=True)
    ended_worker = dorf.end_worker("researcher")
    inspection = dorf.inspect_job("checkout-perf")
    artifacts = dorf.list_job_artifacts("checkout-perf")
    result = dorf.read_job_artifact("checkout-perf", artifacts[0].ref)

    assert ended_worker.worker.status == "ended"
    assert ended_worker.room is not None
    assert inspection.job.status == "ended"
    assert inspection.room.status == "destroyed"
    assert inspection.room_observation == "unavailable"
    assert artifacts[0].job_name == "checkout-perf"
    assert artifacts[0].event_id == "report-finding"
    assert artifacts[0].assignment_id == assignment.binding.assignment.id
    assert artifacts[0].provenance == "claim"
    assert "path" not in asdict(artifacts[0])
    assert result.status == "ok"
    assert result.content == source.read_text()


def test_sdk_exports_exact_large_binary_after_job_and_room_cleanup(
    tmp_path,
    monkeypatch,
) -> None:
    configure_passing_incus(monkeypatch)
    monkeypatch.setattr(
        "dorf.sdk.launch_job_input_dispatcher",
        lambda database, name: False,
    )
    monkeypatch.setattr(
        "dorf.sdk.launch_assignment_report_collector",
        lambda database, job, assignment: False,
    )
    dorf = Dorf.open(
        tmp_path / "state.sqlite3",
        provider_connection="personal-chatgpt",
    )
    dorf.spawn_worker("researcher")
    assignment = dorf.assign_job(
        "checkout-perf",
        worker_name="researcher",
        goal="Retain the binary profile",
    )
    content = bytes(range(256)) * 300
    source = tmp_path / "profile.bin"
    source.write_bytes(content)
    dorf._store.documents.append_event(
        "checkout-perf",
        event_id="report-profile",
        source="worker",
        provenance="claim",
        kind="completion",
        summary="Reported the binary profile",
        related={"assignment": assignment.binding.assignment.id},
        artifacts=[ArtifactInput("profile.bin", source, "application/octet-stream")],
    )

    dorf.end_job("checkout-perf", interrupt=True)
    ended_worker = dorf.end_worker("researcher")
    inspection = dorf.inspect_job("checkout-perf")
    artifact = dorf.list_job_artifacts("checkout-perf")[0]
    model_read = dorf.read_job_artifact("checkout-perf", artifact.ref)
    export_directory = tmp_path / "exports"
    export_directory.mkdir()
    exported = dorf.export_job_artifact(
        "checkout-perf",
        artifact.ref,
        export_directory,
    )
    exported.destination.write_bytes(b"local replacement")
    collision = dorf.export_job_artifact(
        "checkout-perf",
        artifact.ref,
        export_directory,
    )
    local_bytes = exported.destination.read_bytes()
    replaced = dorf.export_job_artifact(
        "checkout-perf",
        artifact.ref,
        export_directory,
        overwrite=True,
    )

    assert ended_worker.room is not None
    assert inspection.job.status == "ended"
    assert inspection.room.status == "destroyed"
    assert model_read.status == "unsupported-media"
    assert exported.status == "ok"
    assert exported.artifact == artifact
    assert exported.destination == export_directory / "profile.bin"
    assert collision.status == "destination-exists"
    assert local_bytes == b"local replacement"
    assert replaced.status == "ok"
    assert replaced.destination.read_bytes() == content
    assert exported.artifact.name == "profile.bin"
    assert exported.artifact.media_type == "application/octet-stream"
    assert exported.artifact.size == 76800
    assert (
        exported.artifact.digest
        == "sha256:f8b0585eb91f58c007a5634362c9f90d8543822c113f702523bc7b73408a9392"
    )


def test_sdk_export_scopes_selection_and_never_publishes_corrupt_or_linked_source(
    tmp_path,
    monkeypatch,
) -> None:
    configure_passing_incus(monkeypatch)
    monkeypatch.setattr("dorf.sdk.launch_job_input_dispatcher", lambda *args: False)
    monkeypatch.setattr("dorf.sdk.launch_assignment_report_collector", lambda *args: False)
    dorf = Dorf.open(
        tmp_path / "state.sqlite3",
        provider_connection="personal-chatgpt",
    )
    dorf.spawn_worker("researcher")
    assignment = dorf.assign_job(
        "checkout-perf",
        worker_name="researcher",
        goal="Retain the profile",
    )
    source = tmp_path / "profile.bin"
    source.write_bytes(b"retained bytes")
    event, _ = dorf._store.documents.append_event(
        "checkout-perf",
        event_id="report-profile",
        source="worker",
        provenance="claim",
        kind="completion",
        summary="Reported the profile",
        related={"assignment": assignment.binding.assignment.id},
        artifacts=[ArtifactInput("profile.bin", source, "application/octet-stream")],
    )
    artifact = dorf.list_job_artifacts("checkout-perf")[0]
    export_directory = tmp_path / "exports"
    export_directory.mkdir()

    missing = dorf.export_job_artifact(
        "checkout-perf",
        f"artifact-v1-{artifact.job_id}-{'0' * 64}",
        export_directory,
    )
    cross_job = dorf.export_job_artifact(
        "checkout-perf",
        f"artifact-v1-{artifact.job_id + 1}-{'0' * 64}",
        export_directory,
    )
    retained = dorf._store.jobs.path("checkout-perf") / event.artifacts[0].path
    retained.write_bytes(b"tampered bytes")
    corrupt = dorf.export_job_artifact(
        "checkout-perf",
        artifact.ref,
        export_directory,
    )
    after_corrupt = list(export_directory.iterdir())
    retained.unlink()
    unrelated = tmp_path / "unrelated.bin"
    unrelated.write_bytes(b"unrelated bytes")
    retained.symlink_to(unrelated)
    linked = dorf.export_job_artifact(
        "checkout-perf",
        artifact.ref,
        export_directory,
    )

    assert missing.status == "missing"
    assert cross_job.status == "cross-job"
    assert corrupt.status == "corrupt"
    assert after_corrupt == []
    assert linked.status == "corrupt"
    assert not (export_directory / "profile.bin").exists()
    assert list(export_directory.iterdir()) == []


def test_sdk_reconstructs_recorded_room_configuration_after_reopen(tmp_path, monkeypatch) -> None:
    commands = configure_passing_incus(monkeypatch)
    database = tmp_path / "state.sqlite3"
    Dorf.open(
        database,
        environment_config=IncusConfig(
            template="recorded-template",
            network="recorded-network",
            root_disk_size="9GiB",
        ),
        provider_connection="personal-chatgpt",
    ).spawn_worker("researcher")
    commands.clear()

    reopened = Dorf.open(database)
    inspection = reopened.inspect_worker("researcher")

    assert inspection.room is not None
    assert inspection.room.metadata["template"] == "recorded-template"
    assert ["incus", "exec", "dorf-researcher", "--", "true"] in commands


def test_sdk_recovery_restarts_replaceable_worker_job_and_report_controllers(
    tmp_path, monkeypatch
) -> None:
    configure_passing_incus(monkeypatch)
    launched: list[tuple[str, str]] = []
    monkeypatch.setattr(
        "dorf.sdk.launch_worker_message_dispatcher",
        lambda database, name: launched.append(("worker", name)) or True,
    )
    monkeypatch.setattr(
        "dorf.sdk.launch_job_input_dispatcher",
        lambda database, name: launched.append(("job", name)) or True,
    )
    monkeypatch.setattr(
        "dorf.sdk.launch_assignment_report_collector",
        lambda database, job, assignment: launched.append(("collector", job)) or True,
    )
    dorf = Dorf.open(
        tmp_path / "state.sqlite3",
        provider_connection="personal-chatgpt",
    )
    dorf.spawn_worker("researcher")
    dorf.assign_job(
        "checkout-perf",
        worker_name="researcher",
        goal="Make checkout instant",
    )
    launched.clear()

    result = dorf.recover_worker("researcher")

    assert result.binding.room.provider_id == "dorf-researcher"
    assert result.room_outcome == "usable"
    assert result.worker_dispatcher_started is True
    assert result.job_dispatcher_started is True
    assert result.report_collector_started is True
    assert launched == [
        ("worker", "researcher"),
        ("job", "checkout-perf"),
        ("collector", "checkout-perf"),
    ]


def test_sdk_interrupt_end_reconciles_stale_job_turn_before_cleanup(tmp_path, monkeypatch) -> None:
    configure_passing_incus(monkeypatch)
    monkeypatch.setattr(
        "dorf.sdk.launch_job_input_dispatcher",
        lambda database, name: False,
    )
    monkeypatch.setattr(
        "dorf.sdk.launch_assignment_report_collector",
        lambda database, job, assignment: False,
    )
    database = tmp_path / "state.sqlite3"
    dorf = Dorf.open(
        database,
        provider_connection="personal-chatgpt",
    )
    dorf.spawn_worker("researcher")
    assignment = dorf.assign_job(
        "checkout-perf",
        worker_name="researcher",
        goal="Make checkout instant",
    )
    store = RuntimeStore.open(database)
    turn, _ = store.admit_job_turn(
        assignment.initial_input,
        output_path="/tmp/stale-job-turn.log",
    )
    conversation = store.get_job_conversation("checkout-perf")
    assert conversation is not None
    store.bind_job_conversation(conversation.id, "thread-job")
    store.prepare_job_turn(turn.id, baseline_native_turn_id=None)
    store.start_job_turn(turn.id, "turn-native")
    store.finish_job_turn(
        turn.id,
        status="recovery-required",
        exit_code=75,
        error="Job harness failure: authentication expired",
    )
    store.close()
    monkeypatch.setattr(
        "dorf.sdk.CodexDriver.recover_job_conversation_turn",
        lambda self, binding, turns, target: AgentTurnRecovery(
            "failed",
            target.native_turn_id,
            target.error,
        ),
    )

    def fail_if_interrupted(self, binding, target):
        raise AssertionError("reconciled failed turn must not be interrupted")

    monkeypatch.setattr(
        "dorf.sdk.CodexDriver.interrupt_job_conversation_turn",
        fail_if_interrupted,
    )

    ended = dorf.end_job("checkout-perf", interrupt=True)

    assert ended.binding.job.status == "ended"
    assert ended.binding.assignment.status == "ended"
