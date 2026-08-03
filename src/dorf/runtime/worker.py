"""Durable Worker identity and recovery of its exact current Room."""

from __future__ import annotations

import re
import secrets
from collections.abc import Callable
from dataclasses import dataclass
from datetime import UTC, datetime
from pathlib import Path
from typing import Protocol

from .agent import AgentTurnRecovery

_WORKER_NAME = re.compile(r"^[a-z0-9][a-z0-9-]{0,62}$")


class InvalidWorkerNameError(ValueError):
    pass


class WorkerOfflineError(RuntimeError):
    pass


class WorkerAlreadyAttachedError(RuntimeError):
    pass


class WorkerUnsettledError(RuntimeError):
    pass


def validate_worker_name(name: str) -> None:
    if not _WORKER_NAME.fullmatch(name):
        raise InvalidWorkerNameError(
            "must start with a lowercase letter or number, contain only lowercase "
            "letters, numbers, and '-', and be at most 63 characters"
        )


@dataclass(frozen=True)
class NewWorker:
    name: str
    provenance: str = "caller"
    lifecycle_policy: str = "caller-managed"
    room_metadata: dict[str, str] | None = None


@dataclass(frozen=True)
class Worker:
    id: int
    name: str
    harness_type: str
    provenance: str
    lifecycle_policy: str
    status: str
    error: str | None
    current_room_id: str | None
    general_conversation_id: str | None
    created_at: str
    updated_at: str


@dataclass(frozen=True)
class Room:
    id: str
    worker_name: str
    room_type: str
    provider_id: str
    workspace: str
    status: str
    error: str | None
    metadata: dict[str, str]
    created_at: str
    updated_at: str


@dataclass(frozen=True)
class WorkerConversation:
    id: str
    worker_name: str
    kind: str
    native_conversation_id: str | None
    model: str
    reasoning_effort: str
    status: str
    error: str | None
    created_at: str
    updated_at: str


@dataclass(frozen=True)
class WorkerMessage:
    id: str
    conversation_id: str
    worker_name: str
    sequence: int
    text: str
    model: str
    reasoning_effort: str
    created_at: str


@dataclass(frozen=True)
class WorkerAgentTurn:
    prompt: str
    output_path: Path
    model: str
    reasoning_effort: str


@dataclass(frozen=True)
class WorkerTurnOutcome:
    native_turn_id: str | None
    status: str
    error: str | None = None


@dataclass(frozen=True)
class WorkerTurn:
    id: int
    conversation_id: str
    worker_name: str
    message_id: str
    status: str
    exit_code: int | None
    output_path: str
    input_digest: str
    native_turn_baseline_id: str | None
    native_turn_id: str | None
    phase: str
    error: str | None
    started_at: str
    finished_at: str | None


@dataclass(frozen=True)
class WorkerWaitResult:
    outcome: str
    observed_at: str
    message_id: str
    sequence: int
    response: str | None = None
    detail: str | None = None


@dataclass(frozen=True)
class WorkerPresence:
    attachment_id: str
    worker_name: str
    room_id: str
    workspace: str
    attached_at: str


@dataclass(frozen=True)
class WorkerAttachResult:
    worker_name: str
    room_id: str
    workspace: str
    exit_code: int


@dataclass(frozen=True)
class WorkerInspection:
    worker: Worker
    room: Room | None
    presence: WorkerPresence | None
    conversation: WorkerConversation | None
    latest_turn: WorkerTurn | None
    queued_messages: int
    observed_at: str
    room_observation: str
    current_job_name: str | None
    room_observation_error: str | None = None


@dataclass(frozen=True)
class WorkerBinding:
    """Current operational view passed to existing environment/harness adapters."""

    worker: Worker
    room: Room

    @property
    def environment_type(self) -> str:
        return self.room.room_type

    @property
    def metadata(self) -> dict[str, str]:
        return self.room.metadata

    @property
    def environment_id(self) -> str:
        return self.room.provider_id

    @property
    def workspace(self) -> str:
        return self.room.workspace

    @property
    def environment_status(self) -> str:
        return self.room.status

    @property
    def environment_error(self) -> str | None:
        return self.room.error

    @property
    def agent_type(self) -> str:
        return self.worker.harness_type

    @property
    def agent_conversation_id(self) -> str | None:
        return self.worker.general_conversation_id

    @property
    def agent_status(self) -> str:
        return self.worker.status

    @property
    def agent_error(self) -> str | None:
        return self.worker.error

    @property
    def end_status(self) -> str:
        return "open"


class WorkerStore(Protocol):
    database_path: Path

    def worker_spawn_lock(self, worker_name: str): ...

    def worker_attachment_lock(self, worker_name: str): ...

    def create_worker_with_room(
        self,
        *,
        name: str,
        harness_type: str,
        provenance: str,
        lifecycle_policy: str,
        room_id: str,
        room_type: str,
        provider_id: str,
        workspace: str,
        metadata: dict[str, str],
    ) -> tuple[WorkerBinding, bool]: ...

    def get_worker(self, name: str) -> Worker | None: ...

    def get_current_room(self, worker_name: str) -> Room | None: ...

    def get_latest_room(self, worker_name: str) -> Room | None: ...

    def mark_worker_room_absent(self, worker_name: str, room_id: str, error: str) -> Worker: ...

    def get_worker_binding(self, name: str) -> WorkerBinding | None: ...

    def get_worker_presence(self, worker_name: str) -> WorkerPresence | None: ...

    def create_worker_presence(
        self,
        worker_name: str,
        *,
        room_id: str,
        attachment_id: str,
        workspace: str,
    ) -> WorkerPresence: ...

    def clear_worker_presence(self, worker_name: str, attachment_id: str) -> bool: ...

    def begin_worker_end(self, worker_name: str, *, interrupt: bool) -> WorkerBinding: ...

    def finish_worker_end(self, worker_name: str, room_id: str) -> Worker: ...

    def update_worker_status(self, name: str, status: str, error: str | None = None) -> Worker: ...

    def update_room_status(self, room_id: str, status: str, error: str | None = None) -> Room: ...

    def get_worker_conversation(self, worker_name: str) -> WorkerConversation | None: ...

    def list_worker_messages(self, worker_name: str) -> list[WorkerMessage]: ...

    def get_worker_turn_by_message(
        self, worker_name: str, message_id: str
    ) -> WorkerTurn | None: ...

    def list_worker_turns(self, worker_name: str) -> list[WorkerTurn]: ...

    def reset_unsubmitted_worker_turn(self, turn_id: int) -> None: ...

    def get_open_job_for_worker(self, worker_name: str) -> str | None: ...

    def admit_worker_turn(
        self, message: WorkerMessage, *, output_path: str
    ) -> tuple[WorkerTurn, bool]: ...

    def bind_worker_conversation(
        self, conversation_id: str, native_conversation_id: str
    ) -> WorkerConversation: ...

    def prepare_worker_turn(
        self, turn_id: int, *, baseline_native_turn_id: str | None
    ) -> WorkerTurn: ...

    def start_worker_turn(self, turn_id: int, native_turn_id: str) -> WorkerTurn: ...

    def finish_worker_turn(
        self,
        turn_id: int,
        *,
        status: str,
        exit_code: int,
        error: str | None,
    ) -> WorkerTurn: ...

    def update_worker_conversation_status(
        self, conversation_id: str, status: str, error: str | None = None
    ) -> WorkerConversation: ...

    def enqueue_worker_message(
        self,
        worker_name: str,
        *,
        message_id: str,
        text: str,
        default_model: str,
        default_reasoning_effort: str,
        model: str | None = None,
        reasoning_effort: str | None = None,
    ) -> tuple[WorkerMessage, bool]: ...


class WorkerEnvironment(Protocol):
    environment_type: str
    workspace: str

    def environment_id(self, worker_name: str) -> str: ...

    def initial_metadata(self, worker_name: str) -> dict[str, str]: ...

    def create(self, binding: WorkerBinding) -> None: ...

    def restore(self, binding: WorkerBinding) -> str: ...

    def stop(self, binding: WorkerBinding) -> str: ...

    def destroy(self, binding: WorkerBinding) -> str: ...

    def execute(self, binding: WorkerBinding, argv: list[str], **kwargs): ...

    def attach(self, binding: WorkerBinding, *, cwd: str) -> int: ...


class WorkerAgent(Protocol):
    agent_type: str

    def prepare(self, binding: WorkerBinding) -> None: ...

    def start_conversation(
        self,
        binding: WorkerBinding,
        turn: WorkerAgentTurn,
        *,
        conversation_started: Callable[[str], None],
        turn_prepared: Callable[[str | None], None],
        turn_started: Callable[[str], None],
    ) -> WorkerTurnOutcome: ...

    def continue_conversation(
        self,
        binding: WorkerBinding,
        turn: WorkerAgentTurn,
        *,
        turn_prepared: Callable[[str | None], None],
        turn_started: Callable[[str], None],
    ) -> WorkerTurnOutcome: ...

    def inspect_conversation(self, binding: WorkerBinding, turns: list[WorkerTurn]): ...

    def interrupt_conversation_turn(
        self, binding: WorkerBinding, turn: WorkerTurn
    ) -> WorkerTurnOutcome: ...

    def recover_conversation_turn(
        self,
        binding: WorkerBinding,
        turns: list[WorkerTurn],
        turn: WorkerTurn,
    ) -> AgentTurnRecovery: ...


class WorkerRuntime:
    """Summon an independent Worker and its current Room without creating a Job."""

    def __init__(
        self,
        store: WorkerStore,
        environment: WorkerEnvironment,
        agent: WorkerAgent,
    ) -> None:
        self._store = store
        self._environment = environment
        self._agent = agent

    def spawn(self, new_worker: NewWorker) -> WorkerBinding:
        validate_worker_name(new_worker.name)
        with self._store.worker_spawn_lock(new_worker.name):
            return self._spawn_locked(new_worker)

    def _spawn_locked(self, new_worker: NewWorker) -> WorkerBinding:
        provider_id = self._environment.environment_id(new_worker.name)
        if not new_worker.provenance or not new_worker.lifecycle_policy:
            raise ValueError("Worker provenance and lifecycle policy cannot be empty")
        metadata = {
            **self._environment.initial_metadata(new_worker.name),
            **(new_worker.room_metadata or {}),
        }
        binding, created = self._store.create_worker_with_room(
            name=new_worker.name,
            harness_type=self._agent.agent_type,
            provenance=new_worker.provenance,
            lifecycle_policy=new_worker.lifecycle_policy,
            room_id=f"room-{secrets.token_hex(16)}",
            room_type=self._environment.environment_type,
            provider_id=provider_id,
            workspace=self._environment.workspace,
            metadata=metadata,
        )
        if (
            binding.room.room_type != self._environment.environment_type
            or binding.worker.harness_type != self._agent.agent_type
        ):
            raise RuntimeError("recorded Worker binding does not match selected adapters")
        if binding.room.status == "ready" and binding.worker.status in {
            "ready",
            "assigned",
        }:
            return binding
        if binding.room.status != "ready":
            if not created:
                self._reconcile_initial_room(binding)
                self._store.update_room_status(binding.room.id, "provisioning")
                binding = self._require_binding(new_worker.name)
            try:
                self._environment.create(binding)
            except Exception as error:
                try:
                    destroyed = self._environment.destroy(binding)
                    if destroyed not in {"deleted", "absent"}:
                        raise RuntimeError(f"Room destroy returned unknown outcome: {destroyed}")
                except Exception as cleanup_error:
                    self._store.update_room_status(
                        binding.room.id,
                        "cleanup-failed",
                        str(cleanup_error),
                    )
                    raise RuntimeError(
                        f"Room provisioning failed and cleanup remains retryable: {cleanup_error}"
                    ) from error
                self._store.update_room_status(binding.room.id, "failed", str(error))
                raise
            self._store.update_room_status(binding.room.id, "ready")
        self._store.update_worker_status(new_worker.name, "preparing")
        binding = self._require_binding(new_worker.name)
        try:
            self._agent.prepare(binding)
        except Exception as error:
            try:
                stopped = self._environment.stop(binding)
                if stopped not in {"stopped", "absent"}:
                    raise RuntimeError(f"Room stop returned unknown outcome: {stopped}")
                destroyed = self._environment.destroy(binding)
                if destroyed not in {"deleted", "absent"}:
                    raise RuntimeError(f"Room destroy returned unknown outcome: {destroyed}")
            except Exception as cleanup_error:
                self._store.update_room_status(
                    binding.room.id,
                    "cleanup-failed",
                    str(cleanup_error),
                )
                self._store.update_worker_status(
                    new_worker.name,
                    "failed",
                    str(cleanup_error),
                )
                raise RuntimeError(
                    f"Worker readiness failed and Room cleanup remains retryable: {cleanup_error}"
                ) from error
            self._store.update_room_status(binding.room.id, "failed", str(error))
            self._store.update_worker_status(new_worker.name, "failed", str(error))
            raise
        self._store.update_worker_status(new_worker.name, "ready")
        return self._require_binding(new_worker.name)

    def _reconcile_initial_room(self, binding: WorkerBinding) -> None:
        stop_outcome = self._environment.stop(binding)
        if stop_outcome == "absent":
            return
        if stop_outcome != "stopped":
            raise RuntimeError(f"Room stop returned unknown outcome: {stop_outcome}")
        destroy_outcome = self._environment.destroy(binding)
        if destroy_outcome not in {"deleted", "absent"}:
            raise RuntimeError(f"Room destroy returned unknown outcome: {destroy_outcome}")

    def end(self, worker_name: str, *, interrupt: bool = False) -> Worker:
        """Destroy the exact idle Worker Room and retain an ended identity tombstone."""
        worker = self._store.get_worker(worker_name)
        if worker is None:
            raise RuntimeError(f"Worker not found: {worker_name}")
        if worker.status == "ended":
            return worker
        open_job = self._store.get_open_job_for_worker(worker_name)
        if open_job is not None:
            raise WorkerUnsettledError(
                f"Worker has open Job {open_job}; end that Job before ending the Worker"
            )
        messages = self._store.list_worker_messages(worker_name)
        turns = self._store.list_worker_turns(worker_name)
        turns_by_message = {turn.message_id: turn for turn in turns}
        unsettled = [
            message
            for message in messages
            if message.id not in turns_by_message
            or turns_by_message[message.id].status in {"running", "recovery-required"}
        ]
        if unsettled and not interrupt:
            message = unsettled[0]
            turn = turns_by_message.get(message.id)
            state = turn.status if turn is not None else "queued"
            raise WorkerUnsettledError(
                f"Worker message {message.sequence} is {state}; use worker wait {worker_name} "
                f"or worker end {worker_name} --interrupt"
            )
        binding = self._store.get_worker_binding(worker_name)
        room_is_lost = binding is None
        if binding is None:
            room = self._store.get_latest_room(worker_name)
            if room is None or room.status not in {"absent", "cleanup-failed"}:
                raise RuntimeError(f"Worker binding not found: {worker_name}")
            binding = WorkerBinding(worker, room)
        if interrupt and not room_is_lost:
            for turn in turns:
                if (
                    turn.status in {"running", "recovery-required"}
                    and turn.native_turn_id is not None
                ):
                    try:
                        self._agent.interrupt_conversation_turn(binding, turn)
                    except Exception:
                        pass
        binding = self._store.begin_worker_end(worker_name, interrupt=interrupt)
        try:
            stopped = self._environment.stop(binding)
            if stopped not in {"stopped", "absent"}:
                raise RuntimeError(f"Room stop returned unknown outcome: {stopped}")
            destroyed = self._environment.destroy(binding)
            if destroyed not in {"deleted", "absent"}:
                raise RuntimeError(f"Room destroy returned unknown outcome: {destroyed}")
        except Exception as error:
            self._store.update_room_status(binding.room.id, "cleanup-failed", str(error))
            self._store.update_worker_status(worker_name, "ending", str(error))
            raise
        return self._store.finish_worker_end(worker_name, binding.room.id)

    def attach(self, worker_name: str) -> WorkerAttachResult:
        """Enter the current Room while recording exact transient human presence."""
        binding = self._store.get_worker_binding(worker_name)
        if binding is None:
            worker = self._store.get_worker(worker_name)
            if worker is None:
                raise RuntimeError(f"Worker not found: {worker_name}")
            raise WorkerOfflineError(f"Worker {worker_name} has no current Room")
        if binding.room.status != "ready" or binding.worker.status not in {
            "ready",
            "assigned",
        }:
            raise WorkerOfflineError(f"Worker {worker_name} is offline")
        attachment_id = f"attachment-{secrets.token_hex(16)}"
        with self._store.worker_attachment_lock(worker_name) as acquired:
            if not acquired:
                raise WorkerAlreadyAttachedError(f"Worker {worker_name} is already attached")
            stale = self._store.get_worker_presence(worker_name)
            if stale is not None:
                self._store.clear_worker_presence(worker_name, stale.attachment_id)
            try:
                presence = self._store.create_worker_presence(
                    worker_name,
                    room_id=binding.room.id,
                    attachment_id=attachment_id,
                    workspace=binding.room.workspace,
                )
                exit_code = self._environment.attach(binding, cwd=presence.workspace)
                return WorkerAttachResult(
                    worker_name,
                    binding.room.id,
                    presence.workspace,
                    exit_code,
                )
            finally:
                self._store.clear_worker_presence(worker_name, attachment_id)

    def recover_room(self, worker_name: str) -> tuple[WorkerBinding, str]:
        """Restore only the exact current Room while its provider body remains."""
        binding = self._store.get_worker_binding(worker_name)
        if binding is None:
            worker = self._store.get_worker(worker_name)
            if worker is None:
                raise RuntimeError(f"Worker not found: {worker_name}")
            raise WorkerOfflineError(
                f"Worker {worker_name} lost its Room; automatic replacement is unsupported"
            )
        outcome = self._environment.restore(binding)
        if outcome == "absent":
            detail = (
                f"Worker {worker_name} Room {binding.room.id} is absent; "
                "automatic replacement is unsupported"
            )
            self._store.mark_worker_room_absent(worker_name, binding.room.id, detail)
            raise WorkerOfflineError(detail)
        if outcome not in {"usable", "restored"}:
            raise RuntimeError(f"Room recovery returned unknown outcome: {outcome}")
        self._store.update_room_status(binding.room.id, "ready")
        assigned = self._store.get_open_job_for_worker(worker_name) is not None
        self._store.update_worker_status(worker_name, "assigned" if assigned else "preparing")
        current = self._require_binding(worker_name)
        try:
            self._agent.prepare(current)
        except Exception as error:
            self._store.update_worker_status(worker_name, "failed", str(error))
            raise
        self._store.update_worker_status(worker_name, "assigned" if assigned else "ready")
        return self._require_binding(worker_name), outcome

    def recover_turns(self, worker_name: str) -> list[WorkerTurn]:
        """Reconcile unsettled general turns without blindly resubmitting input."""
        binding = self._require_binding(worker_name)
        turns = self._store.list_worker_turns(worker_name)
        unsettled = [turn for turn in turns if turn.status in {"running", "recovery-required"}]
        recovered: list[WorkerTurn] = []
        for turn in unsettled:
            conversation = self._store.get_worker_conversation(worker_name)
            if conversation is None:
                raise RuntimeError("Worker general conversation binding is missing")
            if conversation.native_conversation_id is None:
                if turn.native_turn_id is not None:
                    raise RuntimeError("Worker turn has native identity without a conversation")
                self._store.reset_unsubmitted_worker_turn(turn.id)
                self._store.update_worker_conversation_status(conversation.id, "idle")
                continue
            try:
                outcome = self._agent.recover_conversation_turn(binding, turns, turn)
            except Exception as error:
                finished = self._store.finish_worker_turn(
                    turn.id,
                    status="recovery-required",
                    exit_code=75,
                    error=f"Native Worker recovery unavailable: {error}",
                )
                self._store.update_worker_conversation_status(
                    conversation.id, "recovering", finished.error
                )
                recovered.append(finished)
                continue
            if outcome.status == "not-submitted":
                if outcome.native_turn_id is not None or turn.native_turn_id is not None:
                    raise RuntimeError("Harness contradicted the recorded Worker turn identity")
                self._store.reset_unsubmitted_worker_turn(turn.id)
                self._store.update_worker_conversation_status(conversation.id, "idle")
                continue
            native_turn_id = outcome.native_turn_id or turn.native_turn_id
            if native_turn_id is None:
                finished = self._store.finish_worker_turn(
                    turn.id,
                    status="recovery-required",
                    exit_code=75,
                    error=outcome.error or "Native Worker turn identity is uncertain",
                )
                self._store.update_worker_conversation_status(
                    conversation.id, "recovering", finished.error
                )
                recovered.append(finished)
                continue
            self._store.start_worker_turn(turn.id, native_turn_id)
            statuses = {
                "completed": ("succeeded", 0, "idle"),
                "interrupted": ("interrupted", 130, "blocked"),
                "failed": ("failed", 1, "blocked"),
                "active": ("recovery-required", 75, "recovering"),
                "pending-approval": ("recovery-required", 75, "recovering"),
            }
            status, exit_code, conversation_status = statuses.get(
                outcome.status, ("recovery-required", 75, "recovering")
            )
            finished = self._store.finish_worker_turn(
                turn.id,
                status=status,
                exit_code=exit_code,
                error=outcome.error,
            )
            self._store.update_worker_conversation_status(
                conversation.id, conversation_status, outcome.error
            )
            recovered.append(finished)
        return recovered

    def admit_message(
        self,
        worker_name: str,
        *,
        message_id: str,
        text: str,
        default_model: str,
        default_reasoning_effort: str,
        model: str | None = None,
        reasoning_effort: str | None = None,
    ) -> tuple[WorkerMessage, bool]:
        """Durably admit one direct message without requiring an available Room."""
        return self._store.enqueue_worker_message(
            worker_name,
            message_id=message_id,
            text=text,
            default_model=default_model,
            default_reasoning_effort=default_reasoning_effort,
            model=model,
            reasoning_effort=reasoning_effort,
        )

    def observe_wait(self, worker_name: str, message_id: str) -> WorkerWaitResult:
        """Read one exact admitted message outcome and native response."""
        messages = self._store.list_worker_messages(worker_name)
        message = next(
            (candidate for candidate in messages if candidate.id == message_id),
            None,
        )
        if message is None:
            raise RuntimeError(f"Worker message not found: {message_id}")
        observed_at = datetime.now(UTC).isoformat(timespec="microseconds")
        turn = self._store.get_worker_turn_by_message(worker_name, message_id)
        if turn is None:
            inspection = self.inspect(worker_name)
            detail = None
            if inspection.room_observation != "available":
                reason = inspection.room_observation_error or inspection.room_observation
                detail = f"Delivery pending; Worker Room unavailable: {reason}"
            return WorkerWaitResult(
                "working",
                observed_at,
                message.id,
                message.sequence,
                detail=detail,
            )
        terminal_detail = None
        if turn.status in {"failed", "interrupted", "recovery-required"}:
            terminal_detail = turn.error or f"Worker turn is {turn.status}"
        binding = self._store.get_worker_binding(worker_name)
        conversation = self._store.get_worker_conversation(worker_name)
        if binding is None or conversation is None or conversation.native_conversation_id is None:
            outcome = (
                "done"
                if turn.status == "succeeded"
                else "blocked"
                if terminal_detail is not None
                else "working"
            )
            unavailable = "Native Worker response is currently unavailable"
            detail = (
                f"{terminal_detail}; {unavailable}" if terminal_detail is not None else unavailable
            )
            return WorkerWaitResult(
                outcome,
                observed_at,
                message.id,
                message.sequence,
                detail=detail,
            )
        turns = self._store.list_worker_turns(worker_name)
        try:
            inspection = self._agent.inspect_conversation(binding, turns)
        except Exception as error:
            outcome = "done" if turn.status == "succeeded" else "blocked"
            return WorkerWaitResult(
                outcome,
                observed_at,
                message.id,
                message.sequence,
                detail=f"Native Worker response is unavailable: {error}",
            )
        response = _native_turn_response(inspection.native, turn.native_turn_id)
        if inspection.attention_status == "pending-approval":
            outcome = "pending-approval"
            detail = "Worker needs approval in its native conversation"
        elif turn.status == "succeeded":
            outcome = "done"
            detail = None
        elif terminal_detail is not None:
            outcome = "blocked"
            detail = terminal_detail
        else:
            outcome = "working"
            detail = None
        return WorkerWaitResult(
            outcome,
            datetime.now(UTC).isoformat(timespec="microseconds"),
            message.id,
            message.sequence,
            response=response,
            detail=detail,
        )

    def inspect(self, worker_name: str) -> WorkerInspection:
        """Observe durable Worker state and current Room availability without work."""
        worker = self._store.get_worker(worker_name)
        if worker is None:
            raise RuntimeError(f"Worker not found: {worker_name}")
        room = self._store.get_current_room(worker_name)
        conversation = self._store.get_worker_conversation(worker_name)
        presence = self._store.get_worker_presence(worker_name)
        if presence is not None:
            if room is None or presence.room_id != room.id:
                presence = None
            else:
                with self._store.worker_attachment_lock(worker_name) as no_live_owner:
                    if no_live_owner:
                        presence = None
        messages = self._store.list_worker_messages(worker_name)
        turns = self._store.list_worker_turns(worker_name)
        turns_by_message = {turn.message_id: turn for turn in turns}
        queued = sum(
            1
            for message in messages
            if message.id not in turns_by_message
            or turns_by_message[message.id].status in {"running", "recovery-required"}
        )
        observed_at = datetime.now(UTC).isoformat(timespec="microseconds")
        if room is None:
            observation = "absent"
            observation_error = None
        elif room.status != "ready":
            observation = "unavailable"
            observation_error = room.error or f"Room is {room.status}"
        else:
            binding = WorkerBinding(worker, room)
            try:
                result = self._environment.execute(binding, ["true"])
            except Exception as error:
                observation = "unavailable"
                observation_error = str(error)
            else:
                if result.returncode == 0:
                    observation = "available"
                    observation_error = None
                else:
                    observation = "unavailable"
                    observation_error = (
                        result.stderr or result.stdout or "Room is unavailable"
                    ).strip()
        return WorkerInspection(
            worker=worker,
            room=room,
            presence=presence,
            conversation=conversation,
            latest_turn=turns[-1] if turns else None,
            queued_messages=queued,
            observed_at=observed_at,
            room_observation=observation,
            current_job_name=self._store.get_open_job_for_worker(worker_name),
            room_observation_error=observation_error,
        )

    def deliver_message(self, worker_name: str, message_id: str) -> WorkerTurn:
        """Deliver one exact direct message after all earlier general inputs."""
        messages = self._store.list_worker_messages(worker_name)
        target_index = next(
            (index for index, message in enumerate(messages) if message.id == message_id),
            None,
        )
        if target_index is None:
            raise RuntimeError(f"Worker message not found: {message_id}")
        for earlier in messages[:target_index]:
            earlier_turn = self._store.get_worker_turn_by_message(worker_name, earlier.id)
            if earlier_turn is None or earlier_turn.status != "succeeded":
                raise RuntimeError(f"earlier Worker message has not succeeded: {earlier.id}")
        binding = self._require_binding(worker_name)
        if binding.room.status != "ready" or binding.worker.status not in {
            "ready",
            "assigned",
        }:
            raise WorkerOfflineError(f"Worker {worker_name} is offline; message remains queued")
        try:
            availability = self._environment.execute(binding, ["true"])
        except Exception as error:
            raise WorkerOfflineError(
                f"Worker {worker_name} is offline; message remains queued: {error}"
            ) from error
        if availability.returncode != 0:
            detail = (availability.stderr or availability.stdout or "Room is unavailable").strip()
            raise WorkerOfflineError(
                f"Worker {worker_name} is offline; message remains queued: {detail}"
            )
        target = messages[target_index]
        conversation = self._store.get_worker_conversation(worker_name)
        if conversation is None or conversation.id != target.conversation_id:
            raise RuntimeError("Worker general conversation binding is missing")
        output_path = worker_turn_output_path(
            worker_name,
            target.sequence,
            database_path=self._store.database_path,
        )
        turn, admitted = self._store.admit_worker_turn(target, output_path=str(output_path))
        if not admitted or turn.status != "running":
            return turn

        def conversation_started(native_id: str) -> None:
            self._store.bind_worker_conversation(conversation.id, native_id)

        def turn_prepared(baseline: str | None) -> None:
            self._store.prepare_worker_turn(turn.id, baseline_native_turn_id=baseline)
            self._store.update_worker_conversation_status(conversation.id, "running")

        def turn_started(native_id: str) -> None:
            self._store.start_worker_turn(turn.id, native_id)

        launch = WorkerAgentTurn(
            target.text,
            output_path,
            target.model,
            target.reasoning_effort,
        )
        try:
            current_binding = self._require_binding(worker_name)
            if current_binding.agent_conversation_id is None:
                outcome = self._agent.start_conversation(
                    current_binding,
                    launch,
                    conversation_started=conversation_started,
                    turn_prepared=turn_prepared,
                    turn_started=turn_started,
                )
            else:
                outcome = self._agent.continue_conversation(
                    current_binding,
                    launch,
                    turn_prepared=turn_prepared,
                    turn_started=turn_started,
                )
        except Exception as error:
            current = self._store.get_worker_turn_by_message(worker_name, target.id)
            uncertain = current is not None and current.phase in {"submitting", "running"}
            finished = self._store.finish_worker_turn(
                turn.id,
                status="recovery-required" if uncertain else "failed",
                exit_code=75 if uncertain else 126,
                error=f"Worker harness failure: {error}",
            )
            self._store.update_worker_conversation_status(
                conversation.id, "recovering" if uncertain else "blocked", finished.error
            )
            return finished

        statuses = {
            "completed": ("succeeded", 0, "idle"),
            "interrupted": ("interrupted", 130, "blocked"),
            "failed": ("failed", 1, "blocked"),
            "recovery-required": ("recovery-required", 75, "recovering"),
        }
        status, exit_code, conversation_status = statuses.get(
            outcome.status, ("recovery-required", 75, "recovering")
        )
        error = outcome.error
        if outcome.status not in statuses:
            error = f"Worker harness reported unknown turn status: {outcome.status}"
        finished = self._store.finish_worker_turn(
            turn.id,
            status=status,
            exit_code=exit_code,
            error=error,
        )
        self._store.update_worker_conversation_status(conversation.id, conversation_status, error)
        return finished

    def _require_binding(self, name: str) -> WorkerBinding:
        binding = self._store.get_worker_binding(name)
        if binding is None:
            raise RuntimeError(f"Worker binding not found: {name}")
        return binding


def _native_turn_response(native: dict, native_turn_id: str | None) -> str | None:
    turns = native.get("turns")
    if not isinstance(turns, list):
        return None
    native_turn = next(
        (turn for turn in turns if isinstance(turn, dict) and turn.get("id") == native_turn_id),
        None,
    )
    if native_turn is None:
        return None
    items = native_turn.get("items")
    if not isinstance(items, list):
        return None
    responses = [
        item.get("text")
        for item in items
        if isinstance(item, dict)
        and item.get("type") == "agentMessage"
        and isinstance(item.get("text"), str)
        and item.get("text")
    ]
    if not responses:
        return None
    return responses[-1][:8000]


def worker_turn_output_path(
    worker_name: str,
    sequence: int,
    *,
    database_path: Path,
) -> Path:
    return database_path.parent / "runs" / "workers" / worker_name / str(sequence) / "output.log"
