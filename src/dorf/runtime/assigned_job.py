"""Goal-backed Jobs explicitly assigned to independent Workers."""

from __future__ import annotations

import re
from collections.abc import Callable
from dataclasses import dataclass
from datetime import UTC, datetime
from pathlib import Path
from typing import Protocol

from .agent import AgentTurnRecovery
from .job import validate_job_name
from .reporting import (
    assignment_reporting_instructions,
    job_context_root,
    job_report_root,
    report_command_source,
)
from .worker import (
    Room,
    Worker,
    WorkerAgentTurn,
    WorkerBinding,
    WorkerEnvironment,
    WorkerOfflineError,
    WorkerTurnOutcome,
)

_ASSIGNMENT_ID = re.compile(r"^assignment-[a-z0-9][a-z0-9-]{0,120}$")


def validate_assignment_id(value: str) -> None:
    if not _ASSIGNMENT_ID.fullmatch(value):
        raise ValueError("Assignment identity is invalid")


@dataclass(frozen=True)
class NewJob:
    name: str
    worker_name: str
    goal: str
    model: str
    reasoning_effort: str


@dataclass(frozen=True)
class Job:
    id: int
    name: str
    status: str
    goal_version: int
    goal: str
    created_at: str
    updated_at: str


@dataclass(frozen=True)
class Assignment:
    id: str
    job_name: str
    worker_name: str
    generation: int
    status: str
    room_id: str
    workspace: str
    started_at: str
    ended_at: str | None


@dataclass(frozen=True)
class JobConversation:
    id: str
    job_name: str
    native_conversation_id: str | None
    model: str
    reasoning_effort: str
    status: str
    error: str | None
    created_at: str
    updated_at: str


@dataclass(frozen=True)
class JobInput:
    id: str
    conversation_id: str
    job_name: str
    sequence: int
    kind: str
    goal_version: int | None
    text: str
    model: str
    reasoning_effort: str
    created_at: str


@dataclass(frozen=True)
class JobTurn:
    id: int
    conversation_id: str
    job_name: str
    input_id: str
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
class AssignedJobWaitResult:
    outcome: str
    observed_at: str
    input_id: str
    sequence: int
    response: str | None = None
    detail: str | None = None


@dataclass(frozen=True)
class JobInspection:
    job: Job
    assignment: Assignment
    worker: Worker
    room: Room
    conversation: JobConversation
    latest_turn: JobTurn | None
    queued_inputs: int
    observed_at: str
    room_observation: str
    room_observation_error: str | None = None


@dataclass(frozen=True)
class JobBinding:
    job: Job
    assignment: Assignment
    conversation: JobConversation
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
        return self.assignment.workspace

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
        return self.conversation.native_conversation_id

    @property
    def agent_status(self) -> str:
        return self.conversation.status

    @property
    def agent_error(self) -> str | None:
        return self.conversation.error

    @property
    def end_status(self) -> str:
        return self.job.status


class JobAgent(Protocol):
    def start_job_conversation(
        self,
        binding: JobBinding,
        turn: WorkerAgentTurn,
        *,
        conversation_started: Callable[[str], None],
        turn_prepared: Callable[[str | None], None],
        turn_started: Callable[[str], None],
    ) -> WorkerTurnOutcome: ...

    def continue_job_conversation(
        self,
        binding: JobBinding,
        turn: WorkerAgentTurn,
        *,
        turn_prepared: Callable[[str | None], None],
        turn_started: Callable[[str], None],
    ) -> WorkerTurnOutcome: ...

    def inspect_job_conversation(self, binding: JobBinding, turns: list[JobTurn]): ...

    def interrupt_job_conversation_turn(
        self, binding: JobBinding, turn: JobTurn
    ) -> WorkerTurnOutcome: ...

    def recover_job_conversation_turn(
        self,
        binding: JobBinding,
        turns: list[JobTurn],
        turn: JobTurn,
    ) -> AgentTurnRecovery: ...


class JobStore(Protocol):
    database_path: Path

    def job_assignment_lock(self, job_name: str): ...

    def get_worker_binding(self, name: str) -> WorkerBinding | None: ...

    def assign_job(self, new_job: NewJob) -> tuple[JobBinding, bool]: ...

    def get_job_binding(self, name: str) -> JobBinding | None: ...

    def list_job_inputs(self, job_name: str) -> list[JobInput]: ...

    def enqueue_job_input(
        self,
        job_name: str,
        *,
        message_id: str,
        text: str,
        model: str | None = None,
        reasoning_effort: str | None = None,
    ) -> tuple[JobInput, bool]: ...

    def list_job_turns(self, job_name: str) -> list[JobTurn]: ...

    def get_job_turn_by_input(self, job_name: str, input_id: str) -> JobTurn | None: ...

    def reset_unsubmitted_job_turn(self, turn_id: int) -> None: ...

    def admit_job_turn(self, job_input: JobInput, *, output_path: str) -> tuple[JobTurn, bool]: ...

    def bind_job_conversation(
        self, conversation_id: str, native_conversation_id: str
    ) -> JobConversation: ...

    def prepare_job_turn(self, turn_id: int, *, baseline_native_turn_id: str | None) -> JobTurn: ...

    def start_job_turn(self, turn_id: int, native_turn_id: str) -> JobTurn: ...

    def finish_job_turn(
        self,
        turn_id: int,
        *,
        status: str,
        exit_code: int,
        error: str | None,
    ) -> JobTurn: ...

    def update_assignment_status(self, job_name: str, status: str) -> Assignment: ...

    def update_job_conversation_status(
        self, conversation_id: str, status: str, error: str | None = None
    ) -> JobConversation: ...

    def record_job_event(
        self,
        job_name: str,
        *,
        event_id: str,
        source: str,
        provenance: str,
        kind: str,
        summary: str,
        related: dict[str, str],
    ) -> None: ...


class JobRuntime:
    """Assign complete goals to existing ready Workers."""

    def __init__(
        self,
        store: JobStore,
        environment: WorkerEnvironment,
        agent: JobAgent,
    ) -> None:
        self._store = store
        self._environment = environment
        self._agent = agent

    def assign(self, new_job: NewJob, *, activate: bool = True) -> JobBinding:
        validate_job_name(new_job.name)
        if not new_job.goal.strip():
            raise ValueError("Job goal cannot be empty")
        with self._store.job_assignment_lock(new_job.name):
            return self._assign_locked(new_job, activate=activate)

    def _assign_locked(self, new_job: NewJob, *, activate: bool) -> JobBinding:
        existing = self._store.get_job_binding(new_job.name)
        if existing is None:
            worker_binding = self._store.get_worker_binding(new_job.worker_name)
            if worker_binding is None:
                raise RuntimeError(f"Worker not found or Roomless: {new_job.worker_name}")
            if worker_binding.worker.status != "ready" or worker_binding.room.status != "ready":
                raise RuntimeError(f"Worker is not ready: {new_job.worker_name}")
            availability = self._environment.execute(worker_binding, ["true"])
            if availability.returncode != 0:
                detail = (
                    availability.stderr or availability.stdout or "Room is unavailable"
                ).strip()
                raise RuntimeError(f"Worker Room is unavailable: {new_job.worker_name}: {detail}")
        assigned, created = self._store.assign_job(new_job)
        if assigned.assignment.status == "open":
            if activate:
                return assigned
            raise RuntimeError("Job Assignment is already open")
        if assigned.assignment.status not in {"preparing", "workspace-failed"}:
            raise RuntimeError(f"Job Assignment is not assignable: {assigned.assignment.status}")
        if assigned.assignment.status == "workspace-failed":
            self._store.update_assignment_status(assigned.job.name, "preparing")
            assigned = self._store.get_job_binding(assigned.job.name)
            if assigned is None:
                raise RuntimeError("retrying Job binding could not be reloaded")
        room_binding = WorkerBinding(assigned.worker, assigned.room)
        if not created:
            self._environment.execute(
                room_binding,
                ["rm", "-rf", "--", assigned.assignment.workspace],
            )
            self._environment.execute(
                room_binding,
                ["rm", "-rf", "--", f"/run/dorf/jobs/{assigned.job.name}"],
            )
        jobs_root = str(Path(assigned.assignment.workspace).parent)
        root = self._environment.execute(room_binding, ["mkdir", "-p", "--", jobs_root])
        if root.returncode != 0:
            self._store.update_assignment_status(assigned.job.name, "workspace-failed")
            detail = (root.stderr or root.stdout or "workspace root creation failed").strip()
            raise RuntimeError(f"Could not create Job workspace root: {detail}")
        workspace = self._environment.execute(
            room_binding, ["mkdir", "--", assigned.assignment.workspace]
        )
        if workspace.returncode != 0:
            self._store.update_assignment_status(assigned.job.name, "workspace-failed")
            detail = (workspace.stderr or workspace.stdout or "workspace creation failed").strip()
            raise RuntimeError(
                f"Could not create fresh Job workspace {assigned.assignment.workspace}: {detail}"
            )
        try:
            self._prepare_reporting_protocol(assigned)
            initial_input = self._store.list_job_inputs(assigned.job.name)[0]
            self._store.record_job_event(
                assigned.job.name,
                event_id=f"evt-{assigned.assignment.id}",
                source="runtime",
                provenance="fact",
                kind="assignment-started",
                summary="Goal version 1 assigned",
                related={
                    "assignment": assigned.assignment.id,
                    "conversation": assigned.conversation.id,
                    "goal_version": str(assigned.job.goal_version),
                    "input": initial_input.id,
                    "room": assigned.room.id,
                    "worker": assigned.worker.name,
                },
            )
        except Exception as error:
            self._store.update_assignment_status(assigned.job.name, "workspace-failed")
            raise RuntimeError(f"Could not prepare Assignment reporting: {error}") from error
        if activate:
            self._store.update_assignment_status(assigned.job.name, "open")
        ready = self._store.get_job_binding(assigned.job.name)
        if ready is None:
            raise RuntimeError("assigned Job binding could not be reloaded")
        return ready

    def activate_assignment(self, job_name: str) -> JobBinding:
        """Open one fully prepared Assignment for input admission and delivery."""
        with self._store.job_assignment_lock(job_name):
            binding = self._store.get_job_binding(job_name)
            if binding is None:
                raise RuntimeError(f"Job not found: {job_name}")
            if binding.assignment.status == "open":
                return binding
            if binding.assignment.status != "preparing":
                raise RuntimeError(
                    f"Job Assignment is not activatable: {binding.assignment.status}"
                )
            self._store.update_assignment_status(job_name, "open")
            activated = self._store.get_job_binding(job_name)
            if activated is None:
                raise RuntimeError("activated Job binding could not be reloaded")
            return activated

    def _prepare_reporting_protocol(self, binding: JobBinding) -> None:
        room_binding = WorkerBinding(binding.worker, binding.room)
        context = job_context_root(binding.job.name, binding.job.goal_version)
        outbox = job_report_root(binding.job.name)
        self._execute_checked(
            room_binding,
            [
                "mkdir",
                "-p",
                "--",
                context,
                f"{outbox}/tmp",
                f"{outbox}/new",
                f"{outbox}/acks",
            ],
        )
        self._execute_checked(
            room_binding,
            ["tee", "/usr/local/bin/dorf-report"],
            input=report_command_source(),
        )
        self._execute_checked(
            room_binding,
            ["chmod", "0755", "/usr/local/bin/dorf-report"],
        )
        self._execute_checked(
            room_binding,
            ["tee", f"{context}/goal.md"],
            input=f"# Goal\n\n{binding.job.goal}\n",
        )
        instructions = assignment_reporting_instructions(
            binding.job.name,
            binding.assignment.id,
            binding.job.goal_version,
        )
        self._execute_checked(
            room_binding,
            ["tee", f"{context}/REPORTING.md"],
            input=f"# Dorf reporting\n\n{instructions}\n",
        )
        self._execute_checked(
            room_binding,
            [
                "chmod",
                "0444",
                f"{context}/goal.md",
                f"{context}/REPORTING.md",
            ],
        )
        self._execute_checked(room_binding, ["chmod", "0555", context])

    def _execute_checked(
        self,
        binding: WorkerBinding,
        argv: list[str],
        *,
        input: str | None = None,
    ) -> None:
        kwargs = {} if input is None else {"input": input}
        result = self._environment.execute(binding, argv, **kwargs)
        if result.returncode != 0:
            detail = (result.stderr or result.stdout or "Room command failed").strip()
            raise RuntimeError(detail)

    def inspect(self, job_name: str) -> JobInspection:
        """Observe Job, Assignment, and Room facts without creating work."""
        binding = self._store.get_job_binding(job_name)
        if binding is None:
            raise RuntimeError(f"Job not found: {job_name}")
        inputs = self._store.list_job_inputs(job_name)
        turns = self._store.list_job_turns(job_name)
        turns_by_input = {turn.input_id: turn for turn in turns}
        queued = (
            0
            if binding.job.status == "ended"
            else sum(
                1
                for item in inputs
                if item.id not in turns_by_input
                or turns_by_input[item.id].status in {"running", "recovery-required"}
            )
        )
        observation, observation_error = self._observe_room(binding)
        return JobInspection(
            job=binding.job,
            assignment=binding.assignment,
            worker=binding.worker,
            room=binding.room,
            conversation=binding.conversation,
            latest_turn=turns[-1] if turns else None,
            queued_inputs=queued,
            observed_at=datetime.now(UTC).isoformat(timespec="microseconds"),
            room_observation=observation,
            room_observation_error=observation_error,
        )

    def recover_turns(self, job_name: str) -> list[JobTurn]:
        """Reconcile unsettled Job turns without blindly resubmitting input."""
        binding = self._store.get_job_binding(job_name)
        if binding is None:
            raise RuntimeError(f"Job not found: {job_name}")
        turns = self._store.list_job_turns(job_name)
        unsettled = [turn for turn in turns if turn.status in {"running", "recovery-required"}]
        recovered: list[JobTurn] = []
        for turn in unsettled:
            if binding.conversation.native_conversation_id is None:
                if turn.native_turn_id is not None:
                    raise RuntimeError("Job turn has native identity without a conversation")
                self._store.reset_unsubmitted_job_turn(turn.id)
                self._store.update_job_conversation_status(binding.conversation.id, "idle")
                continue
            try:
                outcome = self._agent.recover_job_conversation_turn(binding, turns, turn)
            except Exception as error:
                finished = self._store.finish_job_turn(
                    turn.id,
                    status="recovery-required",
                    exit_code=75,
                    error=f"Native Job recovery unavailable: {error}",
                )
                self._store.update_job_conversation_status(
                    binding.conversation.id, "recovering", finished.error
                )
                recovered.append(finished)
                continue
            if outcome.status == "not-submitted":
                if outcome.native_turn_id is not None or turn.native_turn_id is not None:
                    raise RuntimeError("Harness contradicted the recorded Job turn identity")
                self._store.reset_unsubmitted_job_turn(turn.id)
                self._store.update_job_conversation_status(binding.conversation.id, "idle")
                continue
            native_turn_id = outcome.native_turn_id or turn.native_turn_id
            if native_turn_id is None:
                finished = self._store.finish_job_turn(
                    turn.id,
                    status="recovery-required",
                    exit_code=75,
                    error=outcome.error or "Native Job turn identity is uncertain",
                )
                self._store.update_job_conversation_status(
                    binding.conversation.id, "recovering", finished.error
                )
                recovered.append(finished)
                continue
            self._store.start_job_turn(turn.id, native_turn_id)
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
            finished = self._store.finish_job_turn(
                turn.id,
                status=status,
                exit_code=exit_code,
                error=outcome.error,
            )
            self._store.update_job_conversation_status(
                binding.conversation.id, conversation_status, outcome.error
            )
            recovered.append(finished)
        return recovered

    def observe_wait(self, job_name: str, input_id: str) -> AssignedJobWaitResult:
        """Read one exact Job input outcome and its bounded native response."""
        inputs = self._store.list_job_inputs(job_name)
        target = next((item for item in inputs if item.id == input_id), None)
        if target is None:
            raise RuntimeError(f"Job input not found: {input_id}")
        observed_at = datetime.now(UTC).isoformat(timespec="microseconds")
        turn = self._store.get_job_turn_by_input(job_name, input_id)
        if turn is None:
            inspection = self.inspect(job_name)
            detail = None
            if inspection.room_observation != "available":
                reason = inspection.room_observation_error or inspection.room_observation
                detail = f"Delivery pending; Job Worker Room unavailable: {reason}"
            else:
                turns_by_input = {
                    item.input_id: item for item in self._store.list_job_turns(job_name)
                }
                for earlier in inputs:
                    if earlier.sequence >= target.sequence:
                        break
                    earlier_turn = turns_by_input.get(earlier.id)
                    if earlier_turn is None:
                        detail = f"Delivery pending behind Job input {earlier.sequence}"
                        break
                    if earlier_turn.status == "succeeded":
                        continue
                    reason = earlier_turn.error or earlier_turn.status
                    detail = (
                        f"Delivery blocked by Job input {earlier.sequence} "
                        f"({earlier_turn.status}): {reason}"
                    )
                    break
            return AssignedJobWaitResult(
                "working", observed_at, target.id, target.sequence, detail=detail
            )
        binding = self._store.get_job_binding(job_name)
        terminal_detail = None
        if turn.status in {"failed", "interrupted", "recovery-required"}:
            terminal_detail = turn.error or f"Job turn is {turn.status}"
        if binding is None or binding.conversation.native_conversation_id is None:
            if turn.status == "succeeded":
                outcome = "done"
                detail = "Native Job response is currently unavailable"
            elif terminal_detail is not None:
                outcome = "blocked"
                detail = terminal_detail
            else:
                outcome = "working"
                detail = "Native Job conversation is starting"
            return AssignedJobWaitResult(
                outcome,
                observed_at,
                target.id,
                target.sequence,
                detail=detail,
            )
        turns = self._store.list_job_turns(job_name)
        try:
            native = self._agent.inspect_job_conversation(binding, turns)
        except Exception as error:
            outcome = "done" if turn.status == "succeeded" else "blocked"
            return AssignedJobWaitResult(
                outcome,
                observed_at,
                target.id,
                target.sequence,
                detail=f"Native Job response is unavailable: {error}",
            )
        response = _native_turn_response(native.native, turn.native_turn_id)
        if native.attention_status == "pending-approval":
            outcome = "pending-approval"
            detail = "Job needs approval in its native conversation"
        elif turn.status == "succeeded":
            outcome = "done"
            detail = None
        elif terminal_detail is not None:
            outcome = "blocked"
            detail = terminal_detail
        else:
            outcome = "working"
            detail = None
        return AssignedJobWaitResult(
            outcome,
            datetime.now(UTC).isoformat(timespec="microseconds"),
            target.id,
            target.sequence,
            response=response,
            detail=detail,
        )

    def admit_message(
        self,
        job_name: str,
        *,
        message_id: str,
        text: str,
        model: str | None = None,
        reasoning_effort: str | None = None,
    ) -> tuple[JobInput, bool]:
        """Durably append an ordinary input without changing the pinned goal."""
        job_input, created = self._store.enqueue_job_input(
            job_name,
            message_id=message_id,
            text=text,
            model=model,
            reasoning_effort=reasoning_effort,
        )
        binding = self._store.get_job_binding(job_name)
        if binding is None:
            raise RuntimeError(f"Job not found after input admission: {job_name}")
        self._store.record_job_event(
            job_name,
            event_id=f"evt-{job_input.id}",
            source="client",
            provenance="fact",
            kind="input-admitted",
            summary=f"Job input {job_input.sequence} admitted",
            related={
                "assignment": binding.assignment.id,
                "conversation": binding.conversation.id,
                "input": job_input.id,
                "room": binding.room.id,
                "worker": binding.worker.name,
            },
        )
        return job_input, created

    def _observe_room(self, binding: JobBinding) -> tuple[str, str | None]:
        if binding.worker.current_room_id != binding.assignment.room_id:
            return "unavailable", "Worker is no longer bound to the assigned Room"
        if binding.room.status != "ready":
            return "unavailable", binding.room.error or f"Room is {binding.room.status}"
        try:
            result = self._environment.execute(
                WorkerBinding(binding.worker, binding.room), ["true"]
            )
        except Exception as error:
            return "unavailable", str(error)
        if result.returncode == 0:
            return "available", None
        return (
            "unavailable",
            (result.stderr or result.stdout or "Room is unavailable").strip(),
        )

    def deliver_input(self, job_name: str, input_id: str) -> JobTurn:
        """Deliver one exact input after all earlier inputs in this Job FIFO."""
        inputs = self._store.list_job_inputs(job_name)
        target_index = next(
            (index for index, item in enumerate(inputs) if item.id == input_id),
            None,
        )
        if target_index is None:
            raise RuntimeError(f"Job input not found: {input_id}")
        target = inputs[target_index]
        if target.kind != "cleanup":
            for earlier in inputs[:target_index]:
                earlier_turn = self._store.get_job_turn_by_input(job_name, earlier.id)
                if earlier_turn is None or earlier_turn.status != "succeeded":
                    raise RuntimeError(f"earlier Job input has not succeeded: {earlier.id}")
        binding = self._store.get_job_binding(job_name)
        if binding is None:
            raise RuntimeError(f"Job not found: {job_name}")
        cleanup_delivery = target.kind == "cleanup"
        if (
            binding.job.status not in ({"ending"} if cleanup_delivery else {"open"})
            or binding.assignment.status not in ({"ending"} if cleanup_delivery else {"open"})
            or binding.worker.current_room_id != binding.assignment.room_id
            or binding.room.status != "ready"
        ):
            raise WorkerOfflineError(f"Job {job_name} Worker Room is offline; input remains queued")
        try:
            availability = self._environment.execute(
                WorkerBinding(binding.worker, binding.room), ["true"]
            )
        except Exception as error:
            raise WorkerOfflineError(
                f"Job {job_name} Worker Room is offline; input remains queued: {error}"
            ) from error
        if availability.returncode != 0:
            detail = (availability.stderr or availability.stdout or "Room is unavailable").strip()
            raise WorkerOfflineError(
                f"Job {job_name} Worker Room is offline; input remains queued: {detail}"
            )
        output_path = job_turn_output_path(
            job_name,
            target.sequence,
            database_path=self._store.database_path,
        )
        turn, admitted = self._store.admit_job_turn(target, output_path=str(output_path))
        if not admitted or turn.status != "running":
            return turn

        def conversation_started(native_id: str) -> None:
            conversation = self._store.bind_job_conversation(binding.conversation.id, native_id)
            self._store.record_job_event(
                job_name,
                event_id=f"evt-{conversation.id}",
                source="runtime",
                provenance="fact",
                kind="conversation-started",
                summary="Job native conversation started",
                related={
                    "assignment": binding.assignment.id,
                    "conversation": conversation.id,
                    "native_conversation": native_id,
                    "room": binding.room.id,
                    "worker": binding.worker.name,
                },
            )

        def turn_prepared(baseline: str | None) -> None:
            self._store.prepare_job_turn(turn.id, baseline_native_turn_id=baseline)
            self._store.update_job_conversation_status(binding.conversation.id, "running")

        def turn_started(native_id: str) -> None:
            started = self._store.start_job_turn(turn.id, native_id)
            self._store.record_job_event(
                job_name,
                event_id=f"evt-job-turn-{started.id}-started",
                source="runtime",
                provenance="fact",
                kind="turn-started",
                summary=f"Job input {target.sequence} started",
                related={
                    "assignment": binding.assignment.id,
                    "conversation": binding.conversation.id,
                    "input": target.id,
                    "native_turn": native_id,
                    "room": binding.room.id,
                    "worker": binding.worker.name,
                },
            )

        def record_finished(finished: JobTurn) -> None:
            related = {
                "assignment": binding.assignment.id,
                "conversation": binding.conversation.id,
                "input": target.id,
                "room": binding.room.id,
                "worker": binding.worker.name,
            }
            if finished.native_turn_id is not None:
                related["native_turn"] = finished.native_turn_id
            self._store.record_job_event(
                job_name,
                event_id=f"evt-job-turn-{finished.id}-finished",
                source="runtime",
                provenance="fact",
                kind="turn-finished",
                summary=f"Job input {target.sequence} {finished.status}",
                related=related,
            )

        launch = WorkerAgentTurn(
            target.text,
            output_path,
            target.model,
            target.reasoning_effort,
        )
        try:
            current = self._store.get_job_binding(job_name)
            if current is None:
                raise RuntimeError(f"Job binding not found: {job_name}")
            if current.conversation.native_conversation_id is None:
                outcome = self._agent.start_job_conversation(
                    current,
                    launch,
                    conversation_started=conversation_started,
                    turn_prepared=turn_prepared,
                    turn_started=turn_started,
                )
            else:
                outcome = self._agent.continue_job_conversation(
                    current,
                    launch,
                    turn_prepared=turn_prepared,
                    turn_started=turn_started,
                )
        except Exception as error:
            current_turn = self._store.get_job_turn_by_input(job_name, target.id)
            uncertain = current_turn is not None and current_turn.phase in {
                "submitting",
                "running",
            }
            finished = self._store.finish_job_turn(
                turn.id,
                status="recovery-required" if uncertain else "failed",
                exit_code=75 if uncertain else 126,
                error=f"Job harness failure: {error}",
            )
            self._store.update_job_conversation_status(
                binding.conversation.id,
                "recovering" if uncertain else "blocked",
                finished.error,
            )
            record_finished(finished)
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
            error = f"Job harness reported unknown turn status: {outcome.status}"
        finished = self._store.finish_job_turn(
            turn.id,
            status=status,
            exit_code=exit_code,
            error=error,
        )
        self._store.update_job_conversation_status(
            binding.conversation.id, conversation_status, error
        )
        record_finished(finished)
        return finished


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
    return responses[-1][:8000] if responses else None


def job_turn_output_path(
    job_name: str,
    sequence: int,
    *,
    database_path: Path,
) -> Path:
    validate_job_name(job_name)
    return database_path.parent / "runs" / "jobs" / job_name / str(sequence) / "output.log"
