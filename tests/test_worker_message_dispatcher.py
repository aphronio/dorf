import subprocess
from pathlib import Path

import pytest

from dorf.runtime import NewWorker, RuntimeStore, WorkerRuntime, WorkerTurnOutcome
from dorf.worker_message_dispatcher import (
    _runtime_for_message,
    dispatch_worker_messages,
)


class Environment:
    environment_type = "incus-vm"
    workspace = "/workspace"

    def environment_id(self, name):
        return f"dorf-{name}"

    def initial_metadata(self, name):
        return {"template": "test"}

    def create(self, binding):
        pass

    def execute(self, binding, argv, **kwargs):
        return subprocess.CompletedProcess(argv, 0, "", "")


class Agent:
    agent_type = "codex"

    def __init__(self):
        self.prompts = []

    def prepare(self, binding):
        pass

    def start_conversation(
        self,
        binding,
        turn,
        *,
        conversation_started,
        turn_prepared,
        turn_started,
    ):
        self.prompts.append(turn.prompt)
        conversation_started("thread-general")
        turn_prepared(None)
        turn_started("turn-1")
        return WorkerTurnOutcome("turn-1", "completed")

    def continue_conversation(
        self,
        binding,
        turn,
        *,
        turn_prepared,
        turn_started,
    ):
        self.prompts.append(turn.prompt)
        turn_prepared(f"turn-{len(self.prompts) - 1}")
        turn_started(f"turn-{len(self.prompts)}")
        return WorkerTurnOutcome(f"turn-{len(self.prompts)}", "completed")


def test_runtime_reconstructs_recorded_codex_room_facade(tmp_path: Path, monkeypatch) -> None:
    store = RuntimeStore.open(tmp_path / "state.sqlite3")
    environment = Environment()
    WorkerRuntime(store, environment, Agent()).spawn(NewWorker("researcher"))
    routed_environment = object()
    seen = []

    def reconstruct(binding):
        seen.append(binding)
        return routed_environment

    monkeypatch.setattr(
        "dorf.worker_message_dispatcher.recorded_codex_room_environment",
        reconstruct,
    )

    runtime = _runtime_for_message(store, "researcher")

    assert runtime._environment is routed_environment
    assert seen[0].worker.name == "researcher"


def test_dispatcher_drains_worker_general_fifo_in_order(tmp_path: Path, monkeypatch) -> None:
    database = tmp_path / "state.sqlite3"
    store = RuntimeStore.open(database)
    environment = Environment()
    agent = Agent()
    runtime = WorkerRuntime(store, environment, agent)
    runtime.spawn(NewWorker("researcher"))
    for message_id, text in (("wmsg-1", "one"), ("wmsg-2", "two")):
        runtime.admit_message(
            "researcher",
            message_id=message_id,
            text=text,
            default_model="gpt-5.6-sol",
            default_reasoning_effort="high",
        )
    monkeypatch.setattr(
        "dorf.worker_message_dispatcher._runtime_for_message",
        lambda current_store, worker: WorkerRuntime(current_store, environment, agent),
    )

    dispatch_worker_messages(database, "researcher")

    assert agent.prompts == ["one", "two"]
    assert [turn.status for turn in store.list_worker_turns("researcher")] == [
        "succeeded",
        "succeeded",
    ]


@pytest.mark.parametrize("blocked_status", ["running", "recovery-required"])
def test_dispatcher_leaves_unsettled_message_and_later_fifo_for_recovery_slice(
    tmp_path: Path, monkeypatch, blocked_status
) -> None:
    database = tmp_path / "state.sqlite3"
    store = RuntimeStore.open(database)
    environment = Environment()
    agent = Agent()
    runtime = WorkerRuntime(store, environment, agent)
    runtime.spawn(NewWorker("researcher"))
    message, _ = runtime.admit_message(
        "researcher",
        message_id="wmsg-recovery",
        text="continue",
        default_model="gpt-5.6-sol",
        default_reasoning_effort="high",
    )
    later, _ = runtime.admit_message(
        "researcher",
        message_id="wmsg-later",
        text="later",
        default_model="gpt-5.6-sol",
        default_reasoning_effort="high",
    )
    deliveries = []

    class Runtime:
        def deliver_message(self, worker_name, message_id):
            deliveries.append((worker_name, message_id))
            return type("Outcome", (), {"status": blocked_status})()

    monkeypatch.setattr(
        "dorf.worker_message_dispatcher._runtime_for_message",
        lambda current_store, worker: Runtime(),
    )

    dispatch_worker_messages(database, "researcher")

    assert deliveries == [("researcher", message.id)]
    assert store.get_worker_turn_by_message("researcher", message.id) is None
    assert store.get_worker_turn_by_message("researcher", later.id) is None
    assert [item.id for item in store.list_worker_messages("researcher")] == [
        message.id,
        later.id,
    ]
