import subprocess
from pathlib import Path

import pytest

from dorf.job_input_dispatcher import _runtime_for_input, dispatch_job_inputs
from dorf.runtime import (
    JobRuntime,
    NewJob,
    NewWorker,
    RuntimeStore,
    WorkerRuntime,
    WorkerTurnOutcome,
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

    def start_job_conversation(
        self, binding, turn, *, conversation_started, turn_prepared, turn_started
    ):
        self.prompts.append(turn.prompt)
        conversation_started("thread-job")
        turn_prepared(None)
        turn_started("turn-1")
        return WorkerTurnOutcome("turn-1", "completed")

    def continue_job_conversation(self, binding, turn, *, turn_prepared, turn_started):
        self.prompts.append(turn.prompt)
        turn_prepared(f"turn-{len(self.prompts) - 1}")
        turn_started(f"turn-{len(self.prompts)}")
        return WorkerTurnOutcome(f"turn-{len(self.prompts)}", "completed")


def test_runtime_reconstructs_recorded_codex_room_facade(tmp_path: Path, monkeypatch) -> None:
    store = RuntimeStore.open(tmp_path / "state.sqlite3")
    environment = Environment()
    agent = Agent()
    WorkerRuntime(store, environment, agent).spawn(NewWorker("researcher"))
    JobRuntime(store, environment, agent).assign(
        NewJob(
            "checkout-perf",
            "researcher",
            "Make checkout instant",
            "gpt-5.6-sol",
            "high",
        )
    )
    routed_environment = object()
    seen = []

    def reconstruct(binding):
        seen.append(binding)
        return routed_environment

    monkeypatch.setattr(
        "dorf.job_input_dispatcher.recorded_codex_room_environment",
        reconstruct,
    )

    runtime = _runtime_for_input(store, "checkout-perf")

    assert runtime._environment is routed_environment
    assert seen[0].job.name == "checkout-perf"


def test_dispatcher_drains_job_goal_and_messages_in_order(tmp_path: Path, monkeypatch) -> None:
    database = tmp_path / "state.sqlite3"
    store = RuntimeStore.open(database)
    environment = Environment()
    agent = Agent()
    WorkerRuntime(store, environment, agent).spawn(NewWorker("researcher"))
    runtime = JobRuntime(store, environment, agent)
    runtime.assign(
        NewJob(
            "checkout-perf",
            "researcher",
            "Make checkout instant",
            "gpt-5.6-sol",
            "high",
        )
    )
    runtime.admit_message(
        "checkout-perf",
        message_id="jmsg-2",
        text="Profile the API first",
    )
    monkeypatch.setattr(
        "dorf.job_input_dispatcher._runtime_for_input",
        lambda current_store, job: JobRuntime(current_store, environment, agent),
    )

    dispatch_job_inputs(database, "checkout-perf")

    assert agent.prompts == ["Make checkout instant", "Profile the API first"]
    assert [turn.status for turn in store.list_job_turns("checkout-perf")] == [
        "succeeded",
        "succeeded",
    ]


@pytest.mark.parametrize("blocked_status", ["running", "recovery-required"])
def test_dispatcher_leaves_unsettled_input_and_later_fifo_for_recovery_slice(
    tmp_path: Path, monkeypatch, blocked_status
) -> None:
    database = tmp_path / "state.sqlite3"
    store = RuntimeStore.open(database)
    environment = Environment()
    agent = Agent()
    WorkerRuntime(store, environment, agent).spawn(NewWorker("researcher"))
    runtime = JobRuntime(store, environment, agent)
    runtime.assign(
        NewJob(
            "checkout-perf",
            "researcher",
            "Make checkout instant",
            "gpt-5.6-sol",
            "high",
        )
    )
    later, _ = runtime.admit_message("checkout-perf", message_id="jmsg-later", text="later input")
    first = store.list_job_inputs("checkout-perf")[0]
    deliveries = []

    class Runtime:
        def deliver_input(self, job_name, input_id):
            deliveries.append((job_name, input_id))
            return type("Outcome", (), {"status": blocked_status})()

    monkeypatch.setattr(
        "dorf.job_input_dispatcher._runtime_for_input",
        lambda current_store, job: Runtime(),
    )

    dispatch_job_inputs(database, "checkout-perf")

    assert deliveries == [("checkout-perf", first.id)]
    assert store.get_job_turn_by_input("checkout-perf", first.id) is None
    assert store.get_job_turn_by_input("checkout-perf", later.id) is None
    assert [item.id for item in store.list_job_inputs("checkout-perf")] == [
        first.id,
        later.id,
    ]
