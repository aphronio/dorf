import subprocess
import threading
from pathlib import Path

import pytest

from dorf.runtime import (
    AgentConversationInspection,
    AgentTurnRecovery,
    JobRuntime,
    NewJob,
    NewWorker,
    RuntimeStore,
    WorkerAlreadyAttachedError,
    WorkerOfflineError,
    WorkerRuntime,
    WorkerTurnOutcome,
    WorkerUnsettledError,
)


class RecordingEnvironment:
    environment_type = "incus-vm"
    workspace = "/workspace"

    def __init__(self) -> None:
        self.created = []
        self.executions = []
        self.available = True
        self.create_failure = None
        self.stopped = []
        self.destroyed = []
        self.attachments = []
        self.attach_action = None
        self.restore_outcome = "usable"
        self.destroy_failure = None

    def environment_id(self, worker_name: str) -> str:
        return f"dorf-{worker_name}"

    def initial_metadata(self, worker_name: str) -> dict[str, str]:
        return {"template": "dorf-codex"}

    def create(self, binding) -> None:
        self.created.append(binding)
        if self.create_failure is not None:
            raise self.create_failure

    def stop(self, binding):
        self.stopped.append(binding)
        return "stopped"

    def destroy(self, binding):
        self.destroyed.append(binding)
        if self.destroy_failure is not None:
            raise self.destroy_failure
        return "deleted"

    def execute(self, binding, argv, **kwargs):
        self.executions.append((binding, argv, kwargs))
        return subprocess.CompletedProcess(
            argv,
            0 if self.available else 1,
            "",
            "" if self.available else "Room stopped",
        )

    def restore(self, binding):
        return self.restore_outcome

    def attach(self, binding, *, cwd):
        self.attachments.append((binding, cwd))
        if self.attach_action is not None:
            return self.attach_action()
        return 0


class RecordingAgent:
    agent_type = "codex"

    def __init__(self) -> None:
        self.preparations = []
        self.initial_turns = []
        self.continued_turns = []
        self.inspections = []
        self.recoveries = []
        self.recovery_outcome = AgentTurnRecovery("not-submitted")

    def prepare(self, binding) -> None:
        self.preparations.append(binding)

    def start_conversation(
        self,
        binding,
        turn,
        *,
        conversation_started,
        turn_prepared,
        turn_started,
    ):
        self.initial_turns.append((binding, turn))
        conversation_started("thread-general")
        turn_prepared(None)
        native_id = f"turn-{len(self.initial_turns) + len(self.continued_turns)}"
        turn_started(native_id)
        return WorkerTurnOutcome(native_id, "completed")

    def continue_conversation(
        self,
        binding,
        turn,
        *,
        turn_prepared,
        turn_started,
    ):
        self.continued_turns.append((binding, turn))
        turn_prepared("turn-1")
        native_id = f"turn-{len(self.initial_turns) + len(self.continued_turns)}"
        turn_started(native_id)
        return WorkerTurnOutcome(native_id, "completed")

    def recover_conversation_turn(self, binding, turns, turn):
        self.recoveries.append((binding, turns, turn))
        return self.recovery_outcome

    def inspect_conversation(self, binding, turns):
        self.inspections.append((binding, turns))
        native_turns = [
            {
                "id": turn.native_turn_id,
                "status": "completed",
                "items": [
                    {
                        "type": "agentMessage",
                        "text": f"response for {turn.message_id}",
                    }
                ],
            }
            for turn in turns
            if turn.native_turn_id is not None
        ]
        return AgentConversationInspection(
            "restarted",
            {
                "id": binding.agent_conversation_id,
                "status": {"type": "idle"},
                "turns": native_turns,
            },
        )


def spawn_worker(tmp_path: Path):
    store = RuntimeStore.open(tmp_path / "state.sqlite3")
    environment = RecordingEnvironment()
    agent = RecordingAgent()
    runtime = WorkerRuntime(store, environment, agent)
    binding = runtime.spawn(NewWorker(name="researcher"))
    return store, environment, agent, runtime, binding


def test_spawn_creates_independent_worker_and_room_without_a_job(tmp_path: Path) -> None:
    store = RuntimeStore.open(tmp_path / "state.sqlite3")
    environment = RecordingEnvironment()
    agent = RecordingAgent()
    runtime = WorkerRuntime(store, environment, agent)

    binding = runtime.spawn(NewWorker(name="researcher"))

    assert binding.worker.name == "researcher"
    assert binding.worker.harness_type == "codex"
    assert binding.worker.provenance == "caller"
    assert binding.worker.lifecycle_policy == "caller-managed"
    assert binding.worker.status == "ready"
    assert binding.worker.general_conversation_id is None
    assert binding.room.room_type == "incus-vm"
    assert binding.room.provider_id == "dorf-researcher"
    assert binding.room.workspace == "/workspace"
    assert binding.room.status == "ready"
    assert environment.created[0].room.status == "provisioning"
    assert len(agent.preparations) == 1
    assert store.get_worker("researcher") == binding.worker
    assert store.get_current_room("researcher") == binding.room
    assert store.jobs.get("researcher") is None
    assert not (tmp_path / "jobs" / "researcher").exists()


def test_attach_enters_worker_workspace_and_clears_presence_when_shell_exits(
    tmp_path: Path,
) -> None:
    store, environment, _, runtime, binding = spawn_worker(tmp_path)
    observed = []

    environment.attach_action = lambda: observed.append(runtime.inspect("researcher").presence) or 0

    result = runtime.attach("researcher")

    assert result.exit_code == 0
    assert result.workspace == "/workspace"
    assert environment.attachments == [(binding, "/workspace")]
    assert observed[0].worker_name == "researcher"
    assert observed[0].room_id == binding.room.id
    assert store.get_worker_presence("researcher") is None


def test_attach_clears_presence_when_interrupted_immediately_after_claim(
    tmp_path: Path,
) -> None:
    class InterruptingStore(RuntimeStore):
        def create_worker_presence(self, *args, **kwargs):
            super().create_worker_presence(*args, **kwargs)
            raise KeyboardInterrupt from None

    store = InterruptingStore.open(tmp_path / "state.sqlite3")
    environment = RecordingEnvironment()
    runtime = WorkerRuntime(store, environment, RecordingAgent())
    runtime.spawn(NewWorker("researcher"))

    with pytest.raises(KeyboardInterrupt):
        runtime.attach("researcher")

    assert store.get_worker_presence("researcher") is None
    assert environment.attachments == []


def test_attach_interruption_clears_presence(tmp_path: Path) -> None:
    store, environment, _, runtime, _ = spawn_worker(tmp_path)

    def interrupt():
        assert store.get_worker_presence("researcher") is not None
        raise KeyboardInterrupt

    environment.attach_action = interrupt

    with pytest.raises(KeyboardInterrupt):
        runtime.attach("researcher")

    assert store.get_worker_presence("researcher") is None


def test_attach_fails_honestly_when_worker_room_is_offline(tmp_path: Path) -> None:
    store, environment, _, runtime, binding = spawn_worker(tmp_path)
    store.update_room_status(binding.room.id, "failed", "Room stopped")

    with pytest.raises(WorkerOfflineError, match="offline"):
        runtime.attach("researcher")

    assert environment.attachments == []
    assert store.get_worker_presence("researcher") is None


def test_attach_reclaims_stale_presence_after_client_crash(tmp_path: Path) -> None:
    store, environment, _, runtime, binding = spawn_worker(tmp_path)
    with store.worker_attachment_lock("researcher") as acquired:
        assert acquired is True
        store.create_worker_presence(
            "researcher",
            room_id=binding.room.id,
            attachment_id="attachment-crashed",
            workspace="/workspace",
        )

    assert store.get_worker_presence("researcher") is not None
    assert runtime.inspect("researcher").presence is None
    result = runtime.attach("researcher")

    assert result.exit_code == 0
    assert len(environment.attachments) == 1
    assert store.get_worker_presence("researcher") is None


def test_second_attach_is_rejected_while_human_is_present(tmp_path: Path) -> None:
    database = tmp_path / "state.sqlite3"
    store = RuntimeStore.open(database)
    environment = RecordingEnvironment()
    agent = RecordingAgent()
    WorkerRuntime(store, environment, agent).spawn(NewWorker("researcher"))
    attached = threading.Event()
    release = threading.Event()

    def hold_shell():
        attached.set()
        assert release.wait(timeout=2)
        return 0

    environment.attach_action = hold_shell
    first = threading.Thread(
        target=lambda: WorkerRuntime(RuntimeStore.open(database), environment, agent).attach(
            "researcher"
        )
    )
    first.start()
    assert attached.wait(timeout=2)

    second_runtime = WorkerRuntime(RuntimeStore.open(database), environment, agent)
    with pytest.raises(WorkerAlreadyAttachedError, match="already attached"):
        second_runtime.attach("researcher")

    release.set()
    first.join(timeout=2)
    assert not first.is_alive()
    assert RuntimeStore.open(database).get_worker_presence("researcher") is None


def test_recovery_records_absent_room_without_replacing_worker_job_or_assignment(
    tmp_path: Path,
) -> None:
    store, environment, agent, runtime, original = spawn_worker(tmp_path)
    assigned = JobRuntime(store, environment, agent).assign(
        NewJob(
            "checkout-perf",
            "researcher",
            "Make checkout instant",
            "gpt-5.6-sol",
            "high",
        )
    )
    environment.restore_outcome = "absent"

    with pytest.raises(WorkerOfflineError, match="automatic replacement is unsupported"):
        runtime.recover_room("researcher")

    worker = store.get_worker("researcher")
    retained = store.get_job_binding("checkout-perf")
    assert worker.id == original.worker.id
    assert worker.status == "offline"
    assert worker.current_room_id is None
    assert store.get_room(original.room.id).status == "absent"
    assert retained.job.id == assigned.job.id
    assert retained.conversation.id == assigned.conversation.id
    assert retained.assignment.id == assigned.assignment.id
    assert retained.assignment.generation == 1
    assert len(environment.created) == 1

    message, created = JobRuntime(store, environment, agent).admit_message(
        "checkout-perf",
        message_id="jmsg-after-room-loss",
        text="retain this while offline",
    )
    assert created is True
    assert message.sequence == 2


def test_recovery_restores_the_same_room_identity_when_its_disk_is_usable(
    tmp_path: Path,
) -> None:
    store, environment, agent, runtime, original = spawn_worker(tmp_path)
    store.update_room_status(original.room.id, "failed", "host restarted")
    store.update_worker_status("researcher", "offline")
    environment.restore_outcome = "restored"

    recovered, outcome = runtime.recover_room("researcher")

    assert outcome == "restored"
    assert recovered.worker.id == original.worker.id
    assert recovered.room.id == original.room.id
    assert recovered.room.provider_id == original.room.provider_id
    assert recovered.worker.status == "ready"
    assert recovered.room.status == "ready"
    assert len(agent.preparations) == 2


def test_absent_room_worker_can_end_after_room_loss(tmp_path: Path) -> None:
    store, environment, _, runtime, binding = spawn_worker(tmp_path)
    store.update_room_status(binding.room.id, "absent")
    store.clear_current_room("researcher", binding.room.id)

    ended = runtime.end("researcher")

    assert ended.status == "ended"
    assert ended.current_room_id is None
    assert store.get_room(binding.room.id).status == "destroyed"
    assert [item.room.id for item in environment.stopped] == [binding.room.id]
    assert [item.room.id for item in environment.destroyed] == [binding.room.id]


def test_idle_worker_end_destroys_exact_room_and_retains_tombstone(
    tmp_path: Path,
) -> None:
    store, environment, _, runtime, binding = spawn_worker(tmp_path)

    ended = runtime.end("researcher")
    repeated = runtime.end("researcher")

    assert ended.status == "ended"
    assert ended.current_room_id is None
    assert repeated.id == ended.id
    assert [item.room.id for item in environment.stopped] == [binding.room.id]
    assert [item.room.id for item in environment.destroyed] == [binding.room.id]
    assert store.get_room(binding.room.id).status == "destroyed"
    assert store.get_worker("researcher").id == binding.worker.id


def test_worker_end_retries_exact_room_after_cleanup_failure(tmp_path: Path) -> None:
    store, environment, _, runtime, binding = spawn_worker(tmp_path)
    environment.destroy_failure = RuntimeError("delete failed")

    with pytest.raises(RuntimeError, match="delete failed"):
        runtime.end("researcher")

    assert store.get_worker("researcher").status == "ending"
    assert store.get_room(binding.room.id).status == "cleanup-failed"
    environment.destroy_failure = None
    ended = runtime.end("researcher")
    assert ended.status == "ended"
    assert [item.room.id for item in environment.destroyed] == [
        binding.room.id,
        binding.room.id,
    ]


def test_worker_end_refuses_queued_general_input_without_interrupt(tmp_path: Path) -> None:
    store, _, _, runtime, _ = spawn_worker(tmp_path)
    runtime.admit_message(
        "researcher",
        message_id="wmsg-queued",
        text="pending",
        default_model="gpt-5.6-sol",
        default_reasoning_effort="high",
    )

    with pytest.raises(WorkerUnsettledError, match="worker wait researcher"):
        runtime.end("researcher")

    assert store.get_worker("researcher").status == "ready"


def test_spawn_records_explicit_coding_worker_provenance_and_policy(
    tmp_path: Path,
) -> None:
    store = RuntimeStore.open(tmp_path / "state.sqlite3")
    runtime = WorkerRuntime(store, RecordingEnvironment(), RecordingAgent())

    binding = runtime.spawn(
        NewWorker(
            name="coder-checkout-perf",
            provenance="coding-workflow",
            lifecycle_policy="dedicated",
        )
    )

    assert binding.worker.provenance == "coding-workflow"
    assert binding.worker.lifecycle_policy == "dedicated"
    assert store.get_worker("coder-checkout-perf") == binding.worker


def test_spawn_rejects_conflicting_harness_identity(tmp_path: Path) -> None:
    store, environment, _, _, original = spawn_worker(tmp_path)
    conflicting = RecordingAgent()
    conflicting.agent_type = "pi"

    with pytest.raises(ValueError, match="different harness type"):
        WorkerRuntime(store, environment, conflicting).spawn(NewWorker("researcher"))

    assert store.get_worker_binding("researcher") == original
    assert len(environment.created) == 1
    assert conflicting.preparations == []


def test_concurrent_spawn_callers_provision_one_room(tmp_path: Path) -> None:
    database = tmp_path / "state.sqlite3"

    class BlockingEnvironment(RecordingEnvironment):
        def __init__(self) -> None:
            super().__init__()
            self.create_entered = threading.Event()
            self.release_create = threading.Event()

        def create(self, binding) -> None:
            super().create(binding)
            self.create_entered.set()
            assert self.release_create.wait(timeout=2)

    environment = BlockingEnvironment()
    agent = RecordingAgent()
    results = []
    errors = []

    def spawn() -> None:
        try:
            results.append(
                WorkerRuntime(RuntimeStore.open(database), environment, agent).spawn(
                    NewWorker("researcher")
                )
            )
        except BaseException as error:
            errors.append(error)

    first = threading.Thread(target=spawn)
    second = threading.Thread(target=spawn)
    first.start()
    assert environment.create_entered.wait(timeout=2)
    second.start()
    environment.release_create.set()
    first.join(timeout=2)
    second.join(timeout=2)

    assert not first.is_alive()
    assert not second.is_alive()
    assert errors == []
    assert len(results) == 2
    assert results[0].worker.id == results[1].worker.id
    assert results[0].room.id == results[1].room.id
    assert len(environment.created) == 1
    assert len(agent.preparations) == 1


def test_spawn_retry_reconciles_failed_initial_room_without_new_identity(
    tmp_path: Path,
) -> None:
    store = RuntimeStore.open(tmp_path / "state.sqlite3")
    environment = RecordingEnvironment()
    environment.create_failure = RuntimeError("guest unavailable")
    runtime = WorkerRuntime(store, environment, RecordingAgent())

    with pytest.raises(RuntimeError, match="guest unavailable"):
        runtime.spawn(NewWorker("researcher"))
    failed_worker = store.get_worker("researcher")
    failed_room = store.get_current_room("researcher")
    assert failed_worker is not None
    assert failed_room is not None
    assert failed_room.status == "failed"
    assert len(environment.destroyed) == 1

    environment.create_failure = None
    recovered = runtime.spawn(NewWorker("researcher"))

    assert recovered.worker.id == failed_worker.id
    assert recovered.room.id == failed_room.id
    assert recovered.worker.status == "ready"
    assert recovered.room.status == "ready"
    assert len(environment.stopped) == 1
    assert len(environment.destroyed) == 2
    assert len(environment.created) == 2


def test_recovery_resubmits_only_after_native_history_proves_turn_was_not_submitted(
    tmp_path: Path,
) -> None:
    store, _, agent, runtime, _ = spawn_worker(tmp_path)
    message, _ = runtime.admit_message(
        "researcher",
        message_id="wmsg-recover",
        text="continue safely",
        default_model="gpt-5.6-sol",
        default_reasoning_effort="high",
    )
    turn, _ = store.admit_worker_turn(message, output_path="/tmp/recover.log")
    store.bind_worker_conversation(message.conversation_id, "thread-general")
    store.prepare_worker_turn(turn.id, baseline_native_turn_id=None)

    recovered = runtime.recover_turns("researcher")
    delivered = runtime.deliver_message("researcher", message.id)

    assert recovered == []
    assert delivered.status == "succeeded"
    assert delivered.message_id == message.id
    assert len(agent.recoveries) == 1
    assert len(agent.continued_turns) == 1


def test_transport_loss_after_native_start_requires_reconciliation_before_completion(
    tmp_path: Path,
) -> None:
    store = RuntimeStore.open(tmp_path / "state.sqlite3")
    environment = RecordingEnvironment()

    class DisconnectedAgent(RecordingAgent):
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
            turn_started("turn-native")
            raise RuntimeError("controller disconnected")

    agent = DisconnectedAgent()
    runtime = WorkerRuntime(store, environment, agent)
    runtime.spawn(NewWorker("researcher"))
    message, _ = runtime.admit_message(
        "researcher",
        message_id="wmsg-disconnected",
        text="do this once",
        default_model="gpt-5.6-sol",
        default_reasoning_effort="high",
    )

    uncertain = runtime.deliver_message("researcher", message.id)
    agent.recovery_outcome = AgentTurnRecovery("completed", "turn-native")
    recovered = runtime.recover_turns("researcher")

    assert uncertain.status == "recovery-required"
    assert uncertain.native_turn_id == "turn-native"
    assert recovered[0].status == "succeeded"
    assert recovered[0].native_turn_id == "turn-native"


def test_recovery_binds_native_completion_without_resubmitting_input(
    tmp_path: Path,
) -> None:
    store, _, agent, runtime, _ = spawn_worker(tmp_path)
    message, _ = runtime.admit_message(
        "researcher",
        message_id="wmsg-completed",
        text="do this once",
        default_model="gpt-5.6-sol",
        default_reasoning_effort="high",
    )
    turn, _ = store.admit_worker_turn(message, output_path="/tmp/completed.log")
    store.bind_worker_conversation(message.conversation_id, "thread-general")
    store.prepare_worker_turn(turn.id, baseline_native_turn_id=None)
    agent.recovery_outcome = AgentTurnRecovery("completed", "turn-native")

    recovered = runtime.recover_turns("researcher")

    assert len(recovered) == 1
    assert recovered[0].status == "succeeded"
    assert recovered[0].native_turn_id == "turn-native"
    assert agent.continued_turns == []


def test_worker_messages_are_distinct_fifo_inputs_on_a_lazy_general_conversation(
    tmp_path: Path,
) -> None:
    store, _, _, runtime, binding = spawn_worker(tmp_path)

    first, first_created = runtime.admit_message(
        "researcher",
        message_id="wmsg-first",
        text="hello",
        default_model="gpt-5.6-sol",
        default_reasoning_effort="high",
    )
    second, second_created = runtime.admit_message(
        "researcher",
        message_id="wmsg-second",
        text="hello",
        default_model="ignored-for-existing-conversation",
        default_reasoning_effort="low",
    )

    assert first_created is True
    assert second_created is True
    assert first.sequence == 1
    assert second.sequence == 2
    assert first.text == second.text == "hello"
    assert first.model == second.model == "gpt-5.6-sol"
    assert first.reasoning_effort == second.reasoning_effort == "high"
    conversation = store.get_worker_conversation("researcher")
    assert conversation is not None
    assert conversation.native_conversation_id is None
    assert conversation.model == "gpt-5.6-sol"
    assert conversation.reasoning_effort == "high"
    assert store.list_worker_messages("researcher") == [first, second]
    retried, retry_created = runtime.admit_message(
        "researcher",
        message_id="wmsg-first",
        text="hello",
        default_model="ignored-for-existing-conversation",
        default_reasoning_effort="low",
    )
    assert retry_created is False
    assert retried == first
    with pytest.raises(ValueError, match="different input"):
        runtime.admit_message(
            "researcher",
            message_id="wmsg-first",
            text="changed",
            default_model="ignored-for-existing-conversation",
            default_reasoning_effort="low",
        )

    store.update_room_status(binding.room.id, "failed", "Room offline")
    queued_offline, created_offline = runtime.admit_message(
        "researcher",
        message_id="wmsg-offline",
        text="when you return",
        default_model="ignored-for-existing-conversation",
        default_reasoning_effort="low",
        model="gpt-5.7",
        reasoning_effort="xhigh",
    )

    assert created_offline is True
    assert queued_offline.sequence == 3
    assert queued_offline.model == "gpt-5.7"
    assert queued_offline.reasoning_effort == "xhigh"
    with pytest.raises(WorkerOfflineError, match="remains queued"):
        runtime.deliver_message("researcher", first.id)
    assert store.get_worker_turn_by_message("researcher", first.id) is None


def test_wait_reads_the_selected_response_from_native_history_without_persisting_it(
    tmp_path: Path,
) -> None:
    store, _, agent, runtime, _ = spawn_worker(tmp_path)
    message, _ = runtime.admit_message(
        "researcher",
        message_id="wmsg-selected",
        text="hello",
        default_model="gpt-5.6-sol",
        default_reasoning_effort="high",
    )
    runtime.deliver_message("researcher", message.id)

    result = runtime.observe_wait("researcher", message.id)

    assert result.outcome == "done"
    assert result.message_id == "wmsg-selected"
    assert result.sequence == 1
    assert result.response == "response for wmsg-selected"
    assert len(agent.inspections) == 1
    assert "response for wmsg-selected" not in str(
        store.get_worker_turn_by_message("researcher", message.id)
    )


def test_wait_renders_latest_response_with_pending_native_attention(
    tmp_path: Path,
) -> None:
    database = tmp_path / "state.sqlite3"
    store = RuntimeStore.open(database)
    environment = RecordingEnvironment()

    class BlockingAgent(RecordingAgent):
        def __init__(self) -> None:
            super().__init__()
            self.turn_started_event = threading.Event()
            self.release_turn = threading.Event()

        def start_conversation(
            self,
            binding,
            turn,
            *,
            conversation_started,
            turn_prepared,
            turn_started,
        ):
            self.initial_turns.append((binding, turn))
            conversation_started("thread-general")
            turn_prepared(None)
            turn_started("turn-pending")
            self.turn_started_event.set()
            assert self.release_turn.wait(timeout=2)
            return WorkerTurnOutcome("turn-pending", "completed")

        def inspect_conversation(self, binding, turns):
            return AgentConversationInspection(
                "connected",
                {
                    "id": "thread-general",
                    "status": {
                        "type": "active",
                        "activeFlags": ["waitingOnApproval"],
                    },
                    "turns": [
                        {
                            "id": "turn-pending",
                            "status": "inProgress",
                            "items": [
                                {
                                    "type": "agentMessage",
                                    "text": "I need approval before continuing.",
                                }
                            ],
                        }
                    ],
                },
                attention_status="pending-approval",
            )

    agent = BlockingAgent()
    runtime = WorkerRuntime(store, environment, agent)
    runtime.spawn(NewWorker("researcher"))
    message, _ = runtime.admit_message(
        "researcher",
        message_id="wmsg-pending",
        text="publish it",
        default_model="gpt-5.6-sol",
        default_reasoning_effort="high",
    )
    errors = []

    def deliver() -> None:
        try:
            WorkerRuntime(RuntimeStore.open(database), environment, agent).deliver_message(
                "researcher", message.id
            )
        except BaseException as error:
            errors.append(error)

    delivery = threading.Thread(target=deliver)
    delivery.start()
    assert agent.turn_started_event.wait(timeout=2)
    result = runtime.observe_wait("researcher", message.id)
    agent.release_turn.set()
    delivery.join(timeout=2)

    assert not delivery.is_alive()
    assert errors == []
    assert result.outcome == "pending-approval"
    assert result.response == "I need approval before continuing."
    assert result.detail == "Worker needs approval in its native conversation"


def test_worker_inspection_is_read_only_and_survives_room_unavailability(
    tmp_path: Path,
) -> None:
    store, environment, agent, runtime, _ = spawn_worker(tmp_path)
    message, _ = runtime.admit_message(
        "researcher",
        message_id="wmsg-pending",
        text="hello later",
        default_model="gpt-5.6-sol",
        default_reasoning_effort="high",
    )

    available = runtime.inspect("researcher")
    environment.available = False
    unavailable = runtime.inspect("researcher")
    pending_wait = runtime.observe_wait("researcher", message.id)

    assert available.worker.name == "researcher"
    assert available.room is not None
    assert available.room_observation == "available"
    assert available.conversation is not None
    assert available.conversation.native_conversation_id is None
    assert available.latest_turn is None
    assert available.queued_messages == 1
    assert unavailable.worker == available.worker
    assert unavailable.room == available.room
    assert unavailable.room_observation == "unavailable"
    assert unavailable.room_observation_error == "Room stopped"
    assert pending_wait.outcome == "working"
    assert pending_wait.detail == "Delivery pending; Worker Room unavailable: Room stopped"
    assert store.get_worker_turn_by_message("researcher", message.id) is None
    assert agent.initial_turns == []

    assert available.room is not None
    store.clear_current_room("researcher", available.room.id)
    roomless = runtime.inspect("researcher")
    queued_roomless, created = runtime.admit_message(
        "researcher",
        message_id="wmsg-roomless",
        text="still there?",
        default_model="ignored",
        default_reasoning_effort="low",
    )
    assert roomless.worker.name == "researcher"
    assert roomless.room is None
    assert roomless.room_observation == "absent"
    assert created is True
    assert queued_roomless.sequence == 2


def test_worker_message_fifo_is_transactional_across_clients(tmp_path: Path) -> None:
    database = tmp_path / "state.sqlite3"
    environment = RecordingEnvironment()
    agent = RecordingAgent()
    WorkerRuntime(RuntimeStore.open(database), environment, agent).spawn(NewWorker("researcher"))
    ready = threading.Barrier(2)
    admitted = []
    errors = []

    def admit(message_id: str) -> None:
        try:
            runtime = WorkerRuntime(RuntimeStore.open(database), environment, agent)
            ready.wait(timeout=2)
            admitted.append(
                runtime.admit_message(
                    "researcher",
                    message_id=message_id,
                    text="same text",
                    default_model="gpt-5.6-sol",
                    default_reasoning_effort="high",
                )
            )
        except BaseException as error:
            errors.append(error)

    writers = [
        threading.Thread(target=admit, args=(message_id,)) for message_id in ("wmsg-a", "wmsg-b")
    ]
    for writer in writers:
        writer.start()
    for writer in writers:
        writer.join(timeout=2)

    assert all(not writer.is_alive() for writer in writers)
    assert errors == []
    assert len(admitted) == 2
    messages = RuntimeStore.open(database).list_worker_messages("researcher")
    assert [message.sequence for message in messages] == [1, 2]
    assert {message.id for message in messages} == {"wmsg-a", "wmsg-b"}
    assert len({message.conversation_id for message in messages}) == 1


def test_delivery_starts_one_general_thread_then_continues_it_in_fifo_order(
    tmp_path: Path,
) -> None:
    store, _, agent, runtime, _ = spawn_worker(tmp_path)
    first, _ = runtime.admit_message(
        "researcher",
        message_id="wmsg-first",
        text="introduce yourself",
        default_model="gpt-5.6-sol",
        default_reasoning_effort="high",
    )
    second, _ = runtime.admit_message(
        "researcher",
        message_id="wmsg-second",
        text="now summarize that",
        default_model="ignored",
        default_reasoning_effort="low",
        model="gpt-5.7",
        reasoning_effort="xhigh",
    )

    with pytest.raises(RuntimeError, match="earlier Worker message"):
        runtime.deliver_message("researcher", second.id)

    first_turn = runtime.deliver_message("researcher", first.id)
    second_turn = runtime.deliver_message("researcher", second.id)

    assert first_turn.status == "succeeded"
    assert first_turn.native_turn_id == "turn-1"
    assert second_turn.status == "succeeded"
    assert second_turn.native_turn_id == "turn-2"
    assert len(agent.initial_turns) == 1
    assert len(agent.continued_turns) == 1
    assert agent.initial_turns[0][1].prompt == "introduce yourself"
    assert agent.continued_turns[0][0].agent_conversation_id == "thread-general"
    assert agent.continued_turns[0][1].prompt == "now summarize that"
    assert agent.continued_turns[0][1].model == "gpt-5.7"
    assert agent.continued_turns[0][1].reasoning_effort == "xhigh"
    conversation = store.get_worker_conversation("researcher")
    assert conversation is not None
    assert conversation.native_conversation_id == "thread-general"
    assert conversation.status == "idle"
