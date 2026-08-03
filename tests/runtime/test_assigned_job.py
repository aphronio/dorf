import json
import subprocess
import threading
from pathlib import Path

import pytest

from dorf.runtime import (
    AgentConversationInspection,
    AgentTurnRecovery,
    JobRuntime,
    JobUnsettledError,
    NewJob,
    NewWorker,
    RuntimeStore,
    WorkerOfflineError,
    WorkerRuntime,
    WorkerTurnOutcome,
)


class RecordingEnvironment:
    environment_type = "incus-vm"
    workspace = "/workspace"

    def __init__(self) -> None:
        self.executions = []
        self.available = True
        self.attach_action = None

    def environment_id(self, worker_name: str) -> str:
        return f"dorf-{worker_name}"

    def initial_metadata(self, worker_name: str) -> dict[str, str]:
        return {"template": "dorf-codex"}

    def create(self, binding) -> None:
        pass

    def stop(self, binding):
        return "stopped"

    def destroy(self, binding):
        return "deleted"


class RecordingAgent:
    agent_type = "codex"

    def __init__(self) -> None:
        self.initial_turns = []
        self.general_turns = []
        self.recovery_outcome = AgentTurnRecovery("not-submitted")

    def prepare(self, binding) -> None:
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
        self.general_turns.append((binding, turn))
        conversation_started("thread-general")
        turn_prepared(None)
        turn_started("turn-general")
        return WorkerTurnOutcome("turn-general", "completed")

    def continue_conversation(self, binding, turn, *, turn_prepared, turn_started):
        self.general_turns.append((binding, turn))
        turn_prepared("turn-general")
        turn_started("turn-general-2")
        return WorkerTurnOutcome("turn-general-2", "completed")

    def start_job_conversation(
        self,
        binding,
        turn,
        *,
        conversation_started,
        turn_prepared,
        turn_started,
    ):
        self.initial_turns.append((binding, turn))
        conversation_started("thread-job")
        turn_prepared(None)
        turn_started("turn-goal")
        return WorkerTurnOutcome("turn-goal", "completed")

    def continue_job_conversation(
        self,
        binding,
        turn,
        *,
        turn_prepared,
        turn_started,
    ):
        self.initial_turns.append((binding, turn))
        turn_prepared("turn-goal")
        turn_started("turn-message")
        return WorkerTurnOutcome("turn-message", "completed")

    def recover_job_conversation_turn(self, binding, turns, turn):
        return self.recovery_outcome

    def recover_conversation_turn(self, binding, turns, turn):
        return self.recovery_outcome

    def inspect_job_conversation(self, binding, turns):
        return AgentConversationInspection(
            "restarted",
            {
                "id": binding.agent_conversation_id,
                "status": {"type": "idle"},
                "turns": [
                    {
                        "id": turn.native_turn_id,
                        "status": "completed",
                        "items": [
                            {
                                "type": "agentMessage",
                                "text": f"response for {turn.input_id}",
                            }
                        ],
                    }
                    for turn in turns
                    if turn.native_turn_id is not None
                ],
            },
        )


class JobEnvironment(RecordingEnvironment):
    def execute(self, binding, argv, **kwargs):
        self.executions.append((binding, argv, kwargs))
        return subprocess.CompletedProcess(
            argv,
            0 if self.available else 1,
            "",
            "" if self.available else "Room stopped",
        )

    def attach(self, binding, *, cwd):
        if self.attach_action is not None:
            return self.attach_action(binding, cwd)
        return 0


def ready_worker(tmp_path: Path):
    store = RuntimeStore.open(tmp_path / "state.sqlite3")
    environment = JobEnvironment()
    agent = RecordingAgent()
    WorkerRuntime(store, environment, agent).spawn(NewWorker("researcher"))
    return store, environment, agent


def test_assign_requires_ready_idle_worker_before_creating_any_job_surface(
    tmp_path: Path,
) -> None:
    store = RuntimeStore.open(tmp_path / "state.sqlite3")
    runtime = JobRuntime(store, JobEnvironment(), RecordingAgent())

    with pytest.raises(RuntimeError, match="Worker not found"):
        runtime.assign(
            NewJob(
                name="checkout-perf",
                worker_name="missing",
                goal="Make checkout instant",
                model="gpt-5.6-sol",
                reasoning_effort="high",
            )
        )

    assert store.get_job("checkout-perf") is None
    assert store.get_assignment("checkout-perf") is None
    assert store.list_job_inputs("checkout-perf") == []
    assert store.jobs.get("checkout-perf") is None
    assert not (tmp_path / "jobs" / "checkout-perf").exists()


def test_assign_unavailable_worker_room_leaves_no_job_or_workspace(
    tmp_path: Path,
) -> None:
    store, environment, agent = ready_worker(tmp_path)
    environment.available = False

    with pytest.raises(RuntimeError, match="Worker Room is unavailable"):
        JobRuntime(store, environment, agent).assign(
            NewJob(
                "checkout-perf",
                "researcher",
                "Make checkout instant",
                "gpt-5.6-sol",
                "high",
            )
        )

    assert store.get_job("checkout-perf") is None
    assert store.get_assignment("checkout-perf") is None
    assert store.list_job_inputs("checkout-perf") == []
    assert store.jobs.get("checkout-perf") is None
    assert not any(argv[:1] == ["mkdir"] for _, argv, _ in environment.executions)


def test_assign_atomically_creates_goal_assignment_initial_input_and_workspace(
    tmp_path: Path,
) -> None:
    store, environment, agent = ready_worker(tmp_path)
    runtime = JobRuntime(store, environment, agent)

    binding = runtime.assign(
        NewJob(
            name="checkout-perf",
            worker_name="researcher",
            goal="Make checkout instant",
            model="gpt-5.6-sol",
            reasoning_effort="high",
        )
    )

    assert binding.job.name == "checkout-perf"
    assert binding.job.status == "open"
    assert binding.job.goal_version == 1
    assert binding.job.goal == "Make checkout instant"
    assert binding.assignment.worker_name == "researcher"
    assert binding.assignment.generation == 1
    assert binding.assignment.status == "open"
    assert binding.assignment.room_id == binding.room.id
    assert binding.assignment.workspace == "/workspace/jobs/checkout-perf"
    assert binding.conversation.native_conversation_id is None
    assert binding.conversation.model == "gpt-5.6-sol"
    assert binding.conversation.reasoning_effort == "high"
    inputs = store.list_job_inputs("checkout-perf")
    assert len(inputs) == 1
    assert inputs[0].kind == "goal"
    assert inputs[0].goal_version == 1
    assert inputs[0].sequence == 1
    assert inputs[0].text == "Make checkout instant"
    assert store.get_worker("researcher").status == "assigned"
    document = store.jobs.get("checkout-perf")
    assert document is not None
    assert document.goal["version"] == 1
    assert document.goal["text"] == "Make checkout instant"
    assert document.assignment == {
        "id": binding.assignment.id,
        "generation": 1,
        "worker": "researcher",
        "room": binding.room.id,
        "workspace": binding.assignment.workspace,
    }
    job_json = json.loads((tmp_path / "jobs" / "checkout-perf" / "job.json").read_text())
    assert not {"lifecycle", "status", "error", "worker", "room"} & job_json.keys()
    assert (tmp_path / "jobs" / "checkout-perf" / "goal.md").read_text() == (
        "# Goal\n\nMake checkout instant\n"
    )
    executed = [argv for _, argv, _ in environment.executions]
    assert ["mkdir", "-p", "--", "/workspace/jobs"] in executed
    assert ["mkdir", "--", "/workspace/jobs/checkout-perf"] in executed
    assert agent.initial_turns == []


def test_assignment_projects_read_only_goal_context_and_scoped_report_outbox(
    tmp_path: Path,
) -> None:
    store, environment, agent = ready_worker(tmp_path)

    binding = JobRuntime(store, environment, agent).assign(
        NewJob(
            "checkout-perf",
            "researcher",
            "Make checkout instant",
            "gpt-5.6-sol",
            "high",
        )
    )

    commands = [(argv, kwargs) for _, argv, kwargs in environment.executions]
    context = "/run/dorf/jobs/checkout-perf/context/1"
    outbox = "/run/dorf/jobs/checkout-perf/outbox"
    assert (
        [
            "mkdir",
            "-p",
            "--",
            context,
            f"{outbox}/tmp",
            f"{outbox}/new",
            f"{outbox}/acks",
        ],
        {},
    ) in commands
    assert (
        next(kwargs["input"] for argv, kwargs in commands if argv == ["tee", f"{context}/goal.md"])
        == "# Goal\n\nMake checkout instant\n"
    )
    reporting = next(
        kwargs["input"] for argv, kwargs in commands if argv == ["tee", f"{context}/REPORTING.md"]
    )
    assert context in reporting
    assert binding.assignment.id in reporting
    assert (
        [
            "chmod",
            "0444",
            f"{context}/goal.md",
            f"{context}/REPORTING.md",
        ],
        {},
    ) in commands
    assert (["chmod", "0555", context], {}) in commands
    assert all("/run/dorf/context" not in " ".join(argv) for argv, _ in commands)
    event = store.documents.list_events("checkout-perf")[0]
    initial_input = store.list_job_inputs("checkout-perf")[0]
    assert (event.source, event.provenance, event.kind, event.summary) == (
        "runtime",
        "fact",
        "assignment-started",
        "Goal version 1 assigned",
    )
    assert event.related == {
        "assignment": binding.assignment.id,
        "conversation": binding.conversation.id,
        "goal_version": "1",
        "input": initial_input.id,
        "room": binding.room.id,
        "worker": "researcher",
    }
    assert "Make checkout instant" not in str(event)


def test_concurrent_assignments_to_one_worker_admit_only_one_complete_job(
    tmp_path: Path,
) -> None:
    database = tmp_path / "state.sqlite3"
    store, environment, agent = ready_worker(tmp_path)
    barrier = threading.Barrier(2)
    results = []
    errors = []

    def assign(name: str) -> None:
        try:
            barrier.wait(timeout=2)
            results.append(
                JobRuntime(RuntimeStore.open(database), environment, agent).assign(
                    NewJob(
                        name,
                        "researcher",
                        f"Goal for {name}",
                        "gpt-5.6-sol",
                        "high",
                    )
                )
            )
        except BaseException as error:
            errors.append(error)

    callers = [threading.Thread(target=assign, args=(name,)) for name in ("job-one", "job-two")]
    for caller in callers:
        caller.start()
    for caller in callers:
        caller.join(timeout=2)

    assert all(not caller.is_alive() for caller in callers)
    assert len(results) == 1
    assert len(errors) == 1
    assert "already has open Job" in str(errors[0]) or "not ready" in str(errors[0])
    winner = results[0].job.name
    loser = ({"job-one", "job-two"} - {winner}).pop()
    assert store.get_job(winner) is not None
    assert store.get_assignment(winner) is not None
    assert len(store.list_job_inputs(winner)) == 1
    assert store.get_job(loser) is None
    assert store.get_assignment(loser) is None
    assert store.jobs.get(loser) is None
    assert (
        len(
            [
                argv
                for _, argv, _ in environment.executions
                if argv == ["mkdir", "--", f"/workspace/jobs/{winner}"]
            ]
        )
        == 1
    )


def test_worker_and_job_names_are_typed_and_may_match(tmp_path: Path) -> None:
    store, environment, agent = ready_worker(tmp_path)

    binding = JobRuntime(store, environment, agent).assign(
        NewJob(
            "researcher",
            "researcher",
            "Research the checkout path",
            "gpt-5.6-sol",
            "high",
        )
    )

    assert binding.worker.name == "researcher"
    assert binding.job.name == "researcher"
    assert store.get_worker("researcher") is not None
    assert store.get_job("researcher") is not None


def test_assignment_retry_reconciles_complete_document_published_before_db_commit(
    tmp_path: Path,
) -> None:
    store, environment, agent = ready_worker(tmp_path)
    room = store.get_current_room("researcher")
    assert room is not None
    store.jobs.create_assigned(
        name="checkout-perf",
        goal="Make checkout instant",
        worker_name="researcher",
        room_id=room.id,
        workspace="/workspace/jobs/checkout-perf",
        assignment_id="assignment-before-crash",
        assignment_generation=1,
    )

    binding = JobRuntime(store, environment, agent).assign(
        NewJob(
            "checkout-perf",
            "researcher",
            "Make checkout instant",
            "gpt-5.6-sol",
            "high",
        )
    )

    assert binding.assignment.id == "assignment-before-crash"
    assert binding.assignment.status == "open"
    assert len(store.list_job_inputs("checkout-perf")) == 1


def test_assignment_retry_rebuilds_workspace_after_preparation_failure(
    tmp_path: Path,
) -> None:
    store, environment, agent = ready_worker(tmp_path)
    original_execute = environment.execute
    failed_once = False

    def fail_workspace_once(binding, argv, **kwargs):
        nonlocal failed_once
        if argv == ["mkdir", "--", "/workspace/jobs/checkout-perf"] and not failed_once:
            failed_once = True
            environment.executions.append((binding, argv, kwargs))
            return subprocess.CompletedProcess(argv, 1, "", "disk error")
        return original_execute(binding, argv, **kwargs)

    environment.execute = fail_workspace_once
    runtime = JobRuntime(store, environment, agent)
    request = NewJob(
        "checkout-perf",
        "researcher",
        "Make checkout instant",
        "gpt-5.6-sol",
        "high",
    )

    with pytest.raises(RuntimeError, match="disk error"):
        runtime.assign(request)
    assert store.get_assignment("checkout-perf").status == "workspace-failed"

    recovered = runtime.assign(request)

    assert recovered.assignment.status == "open"
    assert len(store.list_job_inputs("checkout-perf")) == 1
    assert any(
        argv == ["rm", "-rf", "--", "/workspace/jobs/checkout-perf"]
        for _, argv, _ in environment.executions
    )


def test_assignment_retry_rebuilds_partial_context_and_report_scope(
    tmp_path: Path,
) -> None:
    store, environment, agent = ready_worker(tmp_path)
    original_execute = environment.execute
    failed_once = False

    def fail_context_once(binding, argv, **kwargs):
        nonlocal failed_once
        if (
            argv
            == [
                "tee",
                "/run/dorf/jobs/checkout-perf/context/1/goal.md",
            ]
            and not failed_once
        ):
            failed_once = True
            environment.executions.append((binding, argv, kwargs))
            return subprocess.CompletedProcess(argv, 1, "", "context disk error")
        return original_execute(binding, argv, **kwargs)

    environment.execute = fail_context_once
    runtime = JobRuntime(store, environment, agent)
    request = NewJob(
        "checkout-perf",
        "researcher",
        "Make checkout instant",
        "gpt-5.6-sol",
        "high",
    )

    with pytest.raises(RuntimeError, match="context disk error"):
        runtime.assign(request)
    assert store.get_assignment("checkout-perf").status == "workspace-failed"

    recovered = runtime.assign(request)

    assert recovered.assignment.status == "open"
    assert any(
        argv == ["rm", "-rf", "--", "/run/dorf/jobs/checkout-perf"]
        for _, argv, _ in environment.executions
    )
    assignment_events = [
        event
        for event in store.documents.list_events("checkout-perf")
        if event.kind == "assignment-started"
    ]
    assert len(assignment_events) == 1


def test_exact_assignment_retry_reuses_job_assignment_and_initial_input(
    tmp_path: Path,
) -> None:
    store, environment, agent = ready_worker(tmp_path)
    runtime = JobRuntime(store, environment, agent)
    request = NewJob(
        name="checkout-perf",
        worker_name="researcher",
        goal="Make checkout instant",
        model="gpt-5.6-sol",
        reasoning_effort="high",
    )

    first = runtime.assign(request)
    repeated = runtime.assign(request)

    assert repeated == first
    assert len(store.list_job_inputs("checkout-perf")) == 1
    assert (
        len(
            [
                argv
                for _, argv, _ in environment.executions
                if argv == ["mkdir", "--", "/workspace/jobs/checkout-perf"]
            ]
        )
        == 1
    )


def test_initial_job_input_starts_separate_native_thread_with_exact_goal(
    tmp_path: Path,
) -> None:
    store, environment, agent = ready_worker(tmp_path)
    runtime = JobRuntime(store, environment, agent)
    binding = runtime.assign(
        NewJob(
            name="checkout-perf",
            worker_name="researcher",
            goal="Make checkout instant",
            model="gpt-5.6-sol",
            reasoning_effort="high",
        )
    )
    goal_input = store.list_job_inputs("checkout-perf")[0]

    turn = runtime.deliver_input("checkout-perf", goal_input.id)

    assert turn.status == "succeeded"
    assert turn.native_turn_id == "turn-goal"
    assert len(agent.initial_turns) == 1
    launched_binding, launch = agent.initial_turns[0]
    assert launched_binding.workspace == "/workspace/jobs/checkout-perf"
    assert launched_binding.agent_conversation_id is None
    assert launch.prompt == "Make checkout instant"
    assert launch.model == "gpt-5.6-sol"
    assert launch.reasoning_effort == "high"
    conversation = store.get_job_conversation("checkout-perf")
    assert conversation is not None
    assert conversation.native_conversation_id == "thread-job"
    assert conversation.status == "idle"
    assert store.get_worker_conversation("researcher") is None
    assert store.get_job("checkout-perf").status == "open"
    assert store.get_assignment("checkout-perf").status == "open"
    events = store.documents.list_events("checkout-perf")
    conversation_event = next(event for event in events if event.kind == "conversation-started")
    assert conversation_event.related == {
        "assignment": binding.assignment.id,
        "conversation": binding.conversation.id,
        "native_conversation": "thread-job",
        "room": binding.room.id,
        "worker": "researcher",
    }
    started_event = next(event for event in events if event.kind == "turn-started")
    assert started_event.related["input"] == goal_input.id
    assert started_event.related["native_turn"] == "turn-goal"
    finished_event = next(event for event in events if event.kind == "turn-finished")
    assert finished_event.summary == "Job input 1 succeeded"
    assert finished_event.related["native_turn"] == "turn-goal"
    assert "Make checkout instant" not in " ".join(event.summary for event in events)


def test_job_recovery_resubmits_only_after_native_history_proves_no_submission(
    tmp_path: Path,
) -> None:
    store, environment, agent = ready_worker(tmp_path)
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
    goal = store.list_job_inputs("checkout-perf")[0]
    turn, _ = store.admit_job_turn(goal, output_path="/tmp/recover-job.log")
    conversation = store.get_job_conversation("checkout-perf")
    store.bind_job_conversation(conversation.id, "thread-job")
    store.prepare_job_turn(turn.id, baseline_native_turn_id=None)

    recovered = runtime.recover_turns("checkout-perf")
    delivered = runtime.deliver_input("checkout-perf", goal.id)

    assert recovered == []
    assert delivered.status == "succeeded"
    assert delivered.input_id == goal.id
    assert len(agent.initial_turns) == 1


def test_settled_job_end_runs_one_cleanup_turn_and_releases_worker(
    tmp_path: Path,
) -> None:
    store, environment, agent = ready_worker(tmp_path)
    runtime = JobRuntime(store, environment, agent)
    binding = runtime.assign(
        NewJob(
            "checkout-perf",
            "researcher",
            "Make checkout instant",
            "gpt-5.6-sol",
            "high",
        )
    )
    goal = store.list_job_inputs("checkout-perf")[0]
    assert runtime.deliver_input("checkout-perf", goal.id).status == "succeeded"

    ended = runtime.end("checkout-perf")
    repeated = runtime.end("checkout-perf")

    assert ended.job.status == "ended"
    assert ended.assignment.status == "ended"
    assert store.get_worker("researcher").status == "ready"
    assert store.get_open_job_for_worker("researcher") is None
    assert repeated.job.status == "ended"
    cleanup = [item for item in store.list_job_inputs("checkout-perf") if item.kind == "cleanup"]
    assert len(cleanup) == 1
    assert store.get_job_turn_by_input("checkout-perf", cleanup[0].id).status == "succeeded"
    assert any(
        argv
        == [
            "rm",
            "-rf",
            "--",
            binding.assignment.workspace,
            "/run/dorf/jobs/checkout-perf",
        ]
        for _, argv, _ in environment.executions
    )


def test_job_end_retries_workspace_cleanup_without_duplicate_cleanup_turn(
    tmp_path: Path,
) -> None:
    store, environment, agent = ready_worker(tmp_path)
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
    goal = store.list_job_inputs("checkout-perf")[0]
    runtime.deliver_input("checkout-perf", goal.id)
    original_execute = environment.execute
    failures = 0

    def fail_cleanup_once(binding, argv, **kwargs):
        nonlocal failures
        if argv[:3] == ["rm", "-rf", "--"] and failures == 0:
            failures += 1
            return subprocess.CompletedProcess(argv, 1, "", "disk busy")
        return original_execute(binding, argv, **kwargs)

    environment.execute = fail_cleanup_once
    with pytest.raises(RuntimeError, match="disk busy"):
        runtime.end("checkout-perf")
    ended = runtime.end("checkout-perf")

    assert ended.job.status == "ended"
    cleanup = [item for item in store.list_job_inputs("checkout-perf") if item.kind == "cleanup"]
    assert len(cleanup) == 1
    assert (
        len(
            [
                turn
                for turn in store.list_job_turns("checkout-perf")
                if turn.input_id == cleanup[0].id
            ]
        )
        == 1
    )


def test_interrupted_job_end_treats_proven_absent_room_as_local_cleanup(
    tmp_path: Path,
) -> None:
    store, environment, agent = ready_worker(tmp_path)
    runtime = JobRuntime(store, environment, agent)
    binding = runtime.assign(
        NewJob(
            "checkout-perf",
            "researcher",
            "Make checkout instant",
            "gpt-5.6-sol",
            "high",
        )
    )
    store.update_room_status(binding.room.id, "absent")
    store.clear_current_room("researcher", binding.room.id)
    executions_before_end = len(environment.executions)

    ended = runtime.end("checkout-perf", interrupt=True)

    assert ended.job.status == "ended"
    assert ended.assignment.status == "ended"
    assert store.get_worker("researcher").status == "offline"
    assert len(environment.executions) == executions_before_end
    cleanup = [item for item in store.list_job_inputs("checkout-perf") if item.kind == "cleanup"]
    assert len(cleanup) == 1
    assert store.get_job_turn_by_input("checkout-perf", cleanup[0].id) is None


def test_job_end_refuses_unsettled_input_without_interrupt(tmp_path: Path) -> None:
    store, environment, agent = ready_worker(tmp_path)
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

    with pytest.raises(JobUnsettledError, match="job wait checkout-perf"):
        runtime.end("checkout-perf")

    assert store.get_job("checkout-perf").status == "open"


def test_assigned_worker_keeps_an_independent_general_conversation(
    tmp_path: Path,
) -> None:
    store, environment, agent = ready_worker(tmp_path)
    worker_runtime = WorkerRuntime(store, environment, agent)
    direct, _ = worker_runtime.admit_message(
        "researcher",
        message_id="wmsg-general",
        text="Remember our broader discussion",
        default_model="gpt-5.6-sol",
        default_reasoning_effort="high",
    )
    job_runtime = JobRuntime(store, environment, agent)
    job_runtime.assign(
        NewJob(
            "checkout-perf",
            "researcher",
            "Make checkout instant",
            "gpt-5.6-sol",
            "high",
        )
    )

    general_turn = worker_runtime.deliver_message("researcher", direct.id)
    goal = store.list_job_inputs("checkout-perf")[0]
    job_turn = job_runtime.deliver_input("checkout-perf", goal.id)

    assert general_turn.status == "succeeded"
    assert job_turn.status == "succeeded"
    assert store.get_worker_conversation("researcher").native_conversation_id == ("thread-general")
    assert store.get_job_conversation("checkout-perf").native_conversation_id == ("thread-job")
    assert len(agent.general_turns) == 1
    assert len(agent.initial_turns) == 1


def test_human_attachment_does_not_pause_or_rebind_an_active_job_turn(
    tmp_path: Path,
) -> None:
    database = tmp_path / "state.sqlite3"
    environment = JobEnvironment()

    class BlockingJobAgent(RecordingAgent):
        def __init__(self) -> None:
            super().__init__()
            self.job_entered = threading.Event()
            self.release_job = threading.Event()

        def start_job_conversation(
            self,
            binding,
            turn,
            *,
            conversation_started,
            turn_prepared,
            turn_started,
        ):
            conversation_started("thread-job")
            turn_prepared(None)
            turn_started("turn-goal")
            self.job_entered.set()
            assert self.release_job.wait(timeout=2)
            return WorkerTurnOutcome("turn-goal", "completed")

    agent = BlockingJobAgent()
    store = RuntimeStore.open(database)
    worker_runtime = WorkerRuntime(store, environment, agent)
    worker = worker_runtime.spawn(NewWorker("researcher"))
    binding = JobRuntime(store, environment, agent).assign(
        NewJob(
            "checkout-perf",
            "researcher",
            "Job work",
            "gpt-5.6-sol",
            "high",
        )
    )
    goal = store.list_job_inputs("checkout-perf")[0]
    turns = []
    failures = []

    def deliver_job() -> None:
        try:
            turns.append(
                JobRuntime(RuntimeStore.open(database), environment, agent).deliver_input(
                    "checkout-perf", goal.id
                )
            )
        except BaseException as error:
            failures.append(error)

    job_thread = threading.Thread(target=deliver_job)
    job_thread.start()
    assert agent.job_entered.wait(timeout=2)
    observed = []

    def observe_during_attach(actual_binding, cwd):
        inspection = worker_runtime.inspect("researcher")
        observed.append((actual_binding, cwd, inspection.presence))
        return 0

    environment.attach_action = observe_during_attach
    attachment = worker_runtime.attach("researcher")
    active_turn = store.get_job_turn_by_input("checkout-perf", goal.id)

    assert attachment.workspace == "/workspace"
    assert active_turn is not None
    assert active_turn.status == "running"
    assert observed[0][0].worker.id == worker.worker.id
    assert observed[0][0].room.id == worker.room.id
    assert observed[0][1] == "/workspace"
    assert observed[0][2] is not None
    assert observed[0][2].room_id == worker.room.id
    assert store.get_worker_presence("researcher") is None

    agent.release_job.set()
    job_thread.join(timeout=2)
    after = store.get_job_binding("checkout-perf")
    assert not job_thread.is_alive()
    assert failures == []
    assert turns[0].status == "succeeded"
    assert after.job.id == binding.job.id
    assert after.assignment.id == binding.assignment.id
    assert after.worker.id == binding.worker.id
    assert after.room.id == binding.room.id
    assert after.conversation.id == binding.conversation.id


def test_worker_general_and_job_turns_can_run_concurrently(
    tmp_path: Path,
) -> None:
    database = tmp_path / "state.sqlite3"
    environment = JobEnvironment()

    class ConcurrentAgent(RecordingAgent):
        def __init__(self) -> None:
            super().__init__()
            self.general_entered = threading.Event()
            self.job_entered = threading.Event()
            self.release = threading.Event()

        def start_conversation(
            self,
            binding,
            turn,
            *,
            conversation_started,
            turn_prepared,
            turn_started,
        ):
            conversation_started("thread-general")
            turn_prepared(None)
            turn_started("turn-general")
            self.general_entered.set()
            assert self.release.wait(timeout=2)
            return WorkerTurnOutcome("turn-general", "completed")

        def start_job_conversation(
            self,
            binding,
            turn,
            *,
            conversation_started,
            turn_prepared,
            turn_started,
        ):
            conversation_started("thread-job")
            turn_prepared(None)
            turn_started("turn-job")
            self.job_entered.set()
            assert self.release.wait(timeout=2)
            return WorkerTurnOutcome("turn-job", "completed")

    agent = ConcurrentAgent()
    store = RuntimeStore.open(database)
    WorkerRuntime(store, environment, agent).spawn(NewWorker("researcher"))
    worker_runtime = WorkerRuntime(store, environment, agent)
    direct, _ = worker_runtime.admit_message(
        "researcher",
        message_id="wmsg-general",
        text="General work",
        default_model="gpt-5.6-sol",
        default_reasoning_effort="high",
    )
    job_runtime = JobRuntime(store, environment, agent)
    job_runtime.assign(
        NewJob(
            "checkout-perf",
            "researcher",
            "Job work",
            "gpt-5.6-sol",
            "high",
        )
    )
    goal = store.list_job_inputs("checkout-perf")[0]
    outcomes = []
    errors = []

    def deliver_general() -> None:
        try:
            outcomes.append(
                WorkerRuntime(RuntimeStore.open(database), environment, agent).deliver_message(
                    "researcher", direct.id
                )
            )
        except BaseException as error:
            errors.append(error)

    def deliver_job() -> None:
        try:
            outcomes.append(
                JobRuntime(RuntimeStore.open(database), environment, agent).deliver_input(
                    "checkout-perf", goal.id
                )
            )
        except BaseException as error:
            errors.append(error)

    threads = [
        threading.Thread(target=deliver_general),
        threading.Thread(target=deliver_job),
    ]
    for thread in threads:
        thread.start()
    assert agent.general_entered.wait(timeout=2)
    assert agent.job_entered.wait(timeout=2)
    agent.release.set()
    for thread in threads:
        thread.join(timeout=2)

    assert all(not thread.is_alive() for thread in threads)
    assert errors == []
    assert sorted(outcome.status for outcome in outcomes) == [
        "succeeded",
        "succeeded",
    ]


def test_job_message_continues_its_fifo_without_revising_goal(
    tmp_path: Path,
) -> None:
    store, environment, agent = ready_worker(tmp_path)
    runtime = JobRuntime(store, environment, agent)
    job_binding = runtime.assign(
        NewJob(
            name="checkout-perf",
            worker_name="researcher",
            goal="Make checkout instant",
            model="gpt-5.6-sol",
            reasoning_effort="high",
        )
    )
    goal_input = store.list_job_inputs("checkout-perf")[0]
    runtime.deliver_input("checkout-perf", goal_input.id)

    message, created = runtime.admit_message(
        "checkout-perf",
        message_id="jmsg-profile-api",
        text="Profile the API first",
        model="gpt-5.7",
        reasoning_effort="xhigh",
    )
    turn = runtime.deliver_input("checkout-perf", message.id)

    assert created is True
    assert message.sequence == 2
    assert message.kind == "message"
    assert message.goal_version is None
    assert message.model == "gpt-5.7"
    assert message.reasoning_effort == "xhigh"
    message_event = next(
        event
        for event in store.documents.list_events("checkout-perf")
        if event.kind == "input-admitted"
    )
    assert message_event.source == "client"
    assert message_event.provenance == "fact"
    assert message_event.summary == "Job input 2 admitted"
    assert message_event.related["input"] == message.id
    assert message_event.related["assignment"] == job_binding.assignment.id
    assert "Profile the API first" not in str(message_event)
    assert turn.status == "succeeded"
    assert turn.native_turn_id == "turn-message"
    assert len(agent.initial_turns) == 2
    binding, launch = agent.initial_turns[-1]
    assert binding.agent_conversation_id == "thread-job"
    assert launch.prompt == "Profile the API first"
    assert store.get_job("checkout-perf").goal == "Make checkout instant"
    assert store.get_job("checkout-perf").goal_version == 1
    assert store.jobs.get("checkout-perf").goal["text"] == "Make checkout instant"


def test_job_wait_remains_working_while_initial_native_thread_is_being_bound(
    tmp_path: Path,
) -> None:
    store, environment, agent = ready_worker(tmp_path)
    runtime = JobRuntime(store, environment, agent)
    runtime.assign(
        NewJob(
            "checkout-perf",
            "researcher",
            "Publish the result",
            "gpt-5.6-sol",
            "high",
        )
    )
    goal_input = store.list_job_inputs("checkout-perf")[0]
    store.admit_job_turn(goal_input, output_path=str(tmp_path / "output.log"))

    result = runtime.observe_wait("checkout-perf", goal_input.id)

    assert result.outcome == "working"
    assert result.detail == "Native Job conversation is starting"


def test_job_wait_renders_response_with_pending_native_attention(
    tmp_path: Path,
) -> None:
    store, environment, agent = ready_worker(tmp_path)
    runtime = JobRuntime(store, environment, agent)
    runtime.assign(
        NewJob(
            "checkout-perf",
            "researcher",
            "Publish the result",
            "gpt-5.6-sol",
            "high",
        )
    )
    goal_input = store.list_job_inputs("checkout-perf")[0]
    runtime.deliver_input("checkout-perf", goal_input.id)

    agent.inspect_job_conversation = lambda binding, turns: AgentConversationInspection(
        "connected",
        {
            "id": "thread-job",
            "status": {"type": "active", "activeFlags": ["waitingOnApproval"]},
            "turns": [
                {
                    "id": "turn-goal",
                    "status": "inProgress",
                    "items": [
                        {
                            "type": "agentMessage",
                            "text": "I need approval to publish.",
                        }
                    ],
                }
            ],
        },
        attention_status="pending-approval",
    )

    result = runtime.observe_wait("checkout-perf", goal_input.id)

    assert result.outcome == "pending-approval"
    assert result.response == "I need approval to publish."
    assert result.detail == "Job needs approval in its native conversation"


def test_open_job_admits_offline_message_without_native_submission(
    tmp_path: Path,
) -> None:
    store, environment, agent = ready_worker(tmp_path)
    runtime = JobRuntime(store, environment, agent)
    binding = runtime.assign(
        NewJob(
            "checkout-perf",
            "researcher",
            "Make checkout instant",
            "gpt-5.6-sol",
            "high",
        )
    )
    store.update_room_status(binding.room.id, "failed", "Room stopped")

    message, created = runtime.admit_message(
        "checkout-perf",
        message_id="jmsg-offline",
        text="Continue when the Room returns",
    )

    assert created is True
    assert message.sequence == 2
    goal_input = store.list_job_inputs("checkout-perf")[0]
    with pytest.raises(WorkerOfflineError, match="remains queued"):
        runtime.deliver_input("checkout-perf", goal_input.id)
    assert store.get_job_turn_by_input("checkout-perf", goal_input.id) is None
    assert store.get_job_turn_by_input("checkout-perf", message.id) is None
    wait = runtime.observe_wait("checkout-perf", message.id)
    assert wait.outcome == "working"
    assert wait.detail == "Delivery pending; Job Worker Room unavailable: Room stopped"


def test_job_wait_explains_earlier_failed_input_blocking_delivery(tmp_path: Path) -> None:
    store, environment, agent = ready_worker(tmp_path)
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
    goal = store.list_job_inputs("checkout-perf")[0]
    turn, _ = store.admit_job_turn(goal, output_path="/tmp/failed-goal.log")
    store.finish_job_turn(
        turn.id,
        status="failed",
        exit_code=1,
        error="authentication expired",
    )
    followup, _ = runtime.admit_message(
        "checkout-perf",
        message_id="jmsg-followup",
        text="Try the next step",
    )

    wait = runtime.observe_wait("checkout-perf", followup.id)

    assert wait.outcome == "working"
    assert wait.detail == ("Delivery blocked by Job input 1 (failed): authentication expired")


def test_job_inspect_and_wait_are_read_only_views_of_assignment_and_native_response(
    tmp_path: Path,
) -> None:
    store, environment, agent = ready_worker(tmp_path)
    runtime = JobRuntime(store, environment, agent)
    runtime.assign(
        NewJob(
            name="checkout-perf",
            worker_name="researcher",
            goal="Make checkout instant",
            model="gpt-5.6-sol",
            reasoning_effort="high",
        )
    )
    goal_input = store.list_job_inputs("checkout-perf")[0]
    runtime.deliver_input("checkout-perf", goal_input.id)

    inspection = runtime.inspect("checkout-perf")
    result = runtime.observe_wait("checkout-perf", goal_input.id)

    assert inspection.job.goal == "Make checkout instant"
    assert inspection.assignment.worker_name == "researcher"
    assert inspection.room_observation == "available"
    assert inspection.latest_turn is not None
    assert inspection.latest_turn.input_id == goal_input.id
    assert inspection.queued_inputs == 0
    assert result.outcome == "done"
    assert result.input_id == goal_input.id
    assert result.sequence == 1
    assert result.response == f"response for {goal_input.id}"
    assert "response for" not in str(store.get_job_turn_by_input("checkout-perf", goal_input.id))
    assert store.get_job("checkout-perf").status == "open"
    assert store.get_assignment("checkout-perf").status == "open"
