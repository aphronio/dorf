"""Concrete in-process application facade for Dorf resource operations."""

from __future__ import annotations

import hashlib
import secrets
import subprocess
from collections.abc import Callable, Mapping
from dataclasses import dataclass
from pathlib import Path
from time import monotonic, sleep

from dorf.adapters.agents.codex import CodexDriver
from dorf.adapters.agents.codex_config import (
    AgentConfigValidationError as AgentConfigValidationError,
)
from dorf.adapters.agents.codex_config import (
    CodexConfig,
    resolve_codex_config,
)
from dorf.adapters.environments import (
    IncusConfig,
    IncusDoctor,
    IncusEnvironment,
    IncusRunnerProbe,
    incus_bridge_ipv4,
)
from dorf.codex_room import (
    CodexRoomEnvironment,
    RoomRouteGateway,
    new_codex_room_environment,
    recorded_codex_room_environment,
)
from dorf.job_input_dispatcher import launch_job_input_dispatcher
from dorf.provider_gateway import ProviderGateway
from dorf.report_collector import launch_assignment_report_collector
from dorf.runtime import (
    ArtifactExportResult,
    ArtifactReadResult,
    AssignedJobWaitResult,
    JobArtifact,
    JobBinding,
    JobInput,
    JobInspection,
    JobRuntime,
    JobTurn,
    NewJob,
    NewWorker,
    Room,
    RuntimeStore,
    TimelineEvent,
    Worker,
    WorkerAgentTurn,
    WorkerAttachResult,
    WorkerBinding,
    WorkerConversation,
    WorkerInspection,
    WorkerMessage,
    WorkerRuntime,
    WorkerTurn,
    WorkerWaitResult,
)
from dorf.runtime import (
    JobUnsettledError as JobUnsettledError,
)
from dorf.runtime import (
    WorkerUnsettledError as WorkerUnsettledError,
)
from dorf.worker_message_dispatcher import launch_worker_message_dispatcher


def _message_id(prefix: str, action_id: str | None) -> str:
    """Map an opaque caller action onto the runtime's constrained identifier space."""
    if action_id is None:
        return f"{prefix}-{secrets.token_hex(16)}"
    digest = hashlib.sha256(action_id.encode("utf-8")).hexdigest()
    return f"{prefix}-{digest}"


class DorfResourceNotFoundError(RuntimeError):
    """A requested Worker, Job, message, or input does not exist."""


class UnsupportedRoomTypeError(RuntimeError):
    """A recorded Room cannot be operated by this concrete local facade."""


class EnvironmentPrerequisitesError(RuntimeError):
    """The built-in environment cannot safely provision a new Room."""

    def __init__(self, failures: list[str]) -> None:
        self.failures = failures
        super().__init__("; ".join(failures))


class DedicatedWorkerCleanupError(RuntimeError):
    """A Job ended, but cleanup of its dedicated Worker remains retryable."""

    def __init__(self, binding: JobBinding, error: RuntimeError) -> None:
        self.binding = binding
        super().__init__(str(error))


@dataclass(frozen=True)
class WorkerMessageReceipt:
    message: WorkerMessage
    created: bool
    dispatcher_started: bool


@dataclass(frozen=True)
class JobMessageReceipt:
    job_input: JobInput
    created: bool
    dispatcher_started: bool


@dataclass(frozen=True)
class JobAssignmentResult:
    binding: JobBinding
    initial_input: JobInput
    dispatcher_started: bool
    report_collector_started: bool


@dataclass(frozen=True)
class WorkerRecoveryResult:
    binding: WorkerBinding
    room_outcome: str
    worker_turns: tuple[WorkerTurn, ...]
    job_name: str | None
    job_turns: tuple[JobTurn, ...]
    worker_dispatcher_started: bool
    job_dispatcher_started: bool
    report_collector_started: bool


@dataclass(frozen=True)
class WorkerEndResult:
    worker: Worker
    room: Room | None
    already_ended: bool


@dataclass(frozen=True)
class JobEndResult:
    binding: JobBinding
    dedicated_worker: Worker | None


@dataclass(frozen=True)
class JobExecution:
    """Run one recorded Job without exposing runtime or adapter wiring."""

    binding: JobBinding
    _runtime: JobRuntime | None
    _environment: IncusEnvironment | CodexRoomEnvironment
    _agent: CodexDriver
    _git_credential_token: Callable[[str], str] | None

    def _require_runtime(self) -> JobRuntime:
        if self._runtime is None:
            raise RuntimeError("Durable Job runtime operations are unavailable")
        return self._runtime

    def admit_message(
        self,
        *,
        message_id: str,
        text: str,
        model: str | None = None,
        reasoning_effort: str | None = None,
    ) -> tuple[JobInput, bool]:
        return self._require_runtime().admit_message(
            self.binding.job.name,
            message_id=message_id,
            text=text,
            model=model,
            reasoning_effort=reasoning_effort,
        )

    def deliver_input(self, input_id: str) -> JobTurn:
        return self._require_runtime().deliver_input(self.binding.job.name, input_id)

    def execute(
        self,
        argv: list[str],
        *,
        cwd: str | None = None,
        env: Mapping[str, str] | None = None,
        input: str | None = None,
        timeout_seconds: float | None = None,
        provider_route: bool = False,
    ) -> subprocess.CompletedProcess[str]:
        return self._environment.execute(
            self.binding,
            argv,
            cwd=cwd,
            env=env,
            input=input,
            timeout_seconds=timeout_seconds,
            provider_route=provider_route,
        )

    def process_command(
        self,
        argv: list[str],
        *,
        cwd: str | None = None,
        env: Mapping[str, str] | None = None,
        provider_route: bool = False,
    ) -> list[str]:
        return self._environment.process_command(
            self.binding,
            argv,
            cwd=cwd,
            env=env,
            provider_route=provider_route,
        )

    def refresh_git_credentials(self) -> None:
        if self._git_credential_token is None:
            raise RuntimeError("Git credential refresh is unavailable")
        from dorf.coding_workspace import install_git_credentials

        token = self._git_credential_token(self.binding.job.name)
        install_git_credentials(self, self.binding, token=token)

    def run_agent_turn(
        self,
        prompt: str,
        output_path: Path,
        *,
        model: str,
        reasoning_effort: str,
        timeout_seconds: float | None = None,
    ):
        self._agent.prepare(self.binding)
        return self._agent.start_job_conversation(
            self.binding,
            WorkerAgentTurn(prompt, output_path, model, reasoning_effort),
            conversation_started=lambda thread_id: None,
            turn_prepared=lambda baseline: None,
            turn_started=lambda turn_id: None,
            timeout_seconds=timeout_seconds,
        )


@dataclass(frozen=True)
class WorkerExecution:
    binding: WorkerBinding
    _environment: CodexRoomEnvironment

    def execute(self, argv: list[str], **kwargs) -> subprocess.CompletedProcess[str]:
        return self._environment.execute(self.binding, argv, **kwargs)

    def process_command(self, argv: list[str], **kwargs) -> list[str]:
        return self._environment.process_command(self.binding, argv, **kwargs)


class Dorf:
    """Use Dorf locally without going through CLI or network transport."""

    environment_type = IncusEnvironment.environment_type

    def __init__(
        self,
        store: RuntimeStore,
        *,
        environment_config: IncusConfig | None = None,
        agent_defaults: CodexConfig | None = None,
        provider_connection: str | None = None,
        provider_gateway: RoomRouteGateway | None = None,
        git_credential_token: Callable[[str], str] | None = None,
        environment_probe: IncusRunnerProbe | None = None,
    ) -> None:
        self._store = store
        self._environment_config = environment_config or IncusConfig()
        self._agent_defaults = agent_defaults
        self._provider_connection = provider_connection
        self._provider_gateway = provider_gateway
        self._git_credential_token = git_credential_token
        self._environment_probe = environment_probe

    @classmethod
    def open(
        cls,
        database_path: Path | None = None,
        *,
        environment_config: IncusConfig | None = None,
        agent_defaults: CodexConfig | None = None,
        provider_connection: str | None = None,
        provider_gateway: RoomRouteGateway | None = None,
        git_credential_token: Callable[[str], str] | None = None,
        environment_probe: IncusRunnerProbe | None = None,
    ) -> Dorf:
        """Open the local durable authority and compose the built-in adapters."""
        return cls(
            RuntimeStore.open(database_path),
            environment_config=environment_config,
            agent_defaults=agent_defaults,
            provider_connection=provider_connection,
            provider_gateway=provider_gateway,
            git_credential_token=git_credential_token,
            environment_probe=environment_probe,
        )

    def close(self) -> None:
        """Close this facade's durable-store connection."""
        self._store.close()
        close_gateway = getattr(self._provider_gateway, "close", None)
        if callable(close_gateway):
            close_gateway()

    def __enter__(self) -> Dorf:
        return self

    def __exit__(self, *exc_info: object) -> None:
        self.close()

    def get_worker(self, name: str) -> Worker | None:
        return self._store.get_worker(name)

    def get_worker_binding(self, name: str) -> WorkerBinding | None:
        return self._store.get_worker_binding(name)

    def get_worker_conversation(self, name: str) -> WorkerConversation | None:
        return self._store.get_worker_conversation(name)

    def get_job_binding(self, name: str) -> JobBinding | None:
        return self._store.get_job_binding(name)

    def environment_prerequisites(self) -> list[str]:
        return self._new_environment().check_prerequisites()

    @staticmethod
    def new_environment_probe():
        return IncusRunnerProbe()

    @staticmethod
    def check_environment(config: IncusConfig, *, probe=None):
        return IncusDoctor(probe).fast_check(config)

    @staticmethod
    def open_provider_gateway(config: IncusConfig, *, probe=None) -> RoomRouteGateway:
        bind_address = incus_bridge_ipv4(config.network, probe=probe)
        return ProviderGateway.open(bind_address=bind_address)

    def spawn_worker(
        self,
        name: str,
        *,
        provenance: str = "caller",
        lifecycle_policy: str = "caller-managed",
        room_metadata: dict[str, str] | None = None,
    ) -> WorkerBinding:
        existing = self._store.get_worker_binding(name)
        if existing is None and self._provider_connection is None:
            raise ValueError("A Provider Connection is required to create a new Worker Room")
        environment = (
            self._environment_for_binding(existing)
            if existing is not None
            else self._new_environment()
        )
        failures = environment.check_prerequisites()
        if failures:
            raise EnvironmentPrerequisitesError(failures)
        return self._worker_runtime(environment).spawn(
            NewWorker(name, provenance, lifecycle_policy, room_metadata)
        )

    def end_worker(self, name: str, *, interrupt: bool = False) -> WorkerEndResult:
        worker = self._store.get_worker(name)
        if worker is None:
            raise DorfResourceNotFoundError(f"Worker not found: {name}")
        if worker.status == "ended":
            return WorkerEndResult(worker, None, True)
        room = self._store.get_current_room(name) or self._store.get_latest_room(name)
        if room is None or (
            worker.current_room_id is None and room.status not in {"absent", "cleanup-failed"}
        ):
            raise RuntimeError("current Room is missing")
        binding = WorkerBinding(worker, room)
        runtime = self._worker_runtime(self._environment_for_binding(binding))
        if interrupt:
            runtime.recover_turns(name)
        ended = runtime.end(name, interrupt=interrupt)
        return WorkerEndResult(ended, room, False)

    def recover_worker(self, name: str) -> WorkerRecoveryResult:
        binding = self._store.get_worker_binding(name)
        if binding is None:
            worker = self._store.get_worker(name)
            detail = (
                "not found"
                if worker is None
                else "its Room was lost and automatic replacement is unsupported"
            )
            raise DorfResourceNotFoundError(f"Could not recover Worker {name}: {detail}")
        environment = self._environment_for_binding(binding)
        agent = CodexDriver(environment)
        worker_runtime = WorkerRuntime(self._store, environment, agent)
        binding, room_outcome = worker_runtime.recover_room(name)
        worker_turns = tuple(worker_runtime.recover_turns(name))
        job_name = self._store.get_open_job_for_worker(name)
        job_turns = (
            tuple(self._job_runtime(environment, agent).recover_turns(job_name))
            if job_name
            else ()
        )
        worker_started = launch_worker_message_dispatcher(self._store.database_path, name)
        job_started = (
            launch_job_input_dispatcher(self._store.database_path, job_name) if job_name else False
        )
        collector_started = False
        if job_name:
            current = self._store.get_job_binding(job_name)
            if current is not None:
                collector_started = launch_assignment_report_collector(
                    self._store.database_path,
                    current.job.name,
                    current.assignment.id,
                )
        return WorkerRecoveryResult(
            binding=binding,
            room_outcome=room_outcome,
            worker_turns=worker_turns,
            job_name=job_name,
            job_turns=job_turns,
            worker_dispatcher_started=worker_started,
            job_dispatcher_started=job_started,
            report_collector_started=collector_started,
        )

    def attach_worker(self, name: str) -> WorkerAttachResult:
        binding = self._require_worker_binding(name)
        return self._worker_runtime(self._environment_for_binding(binding)).attach(name)

    def inspect_worker(self, name: str) -> WorkerInspection:
        worker = self._store.get_worker(name)
        if worker is None:
            raise DorfResourceNotFoundError(f"Worker not found: {name}")
        room = self._store.get_current_room(name)
        environment = (
            self._new_environment()
            if room is None
            else self._environment_for_binding(WorkerBinding(worker, room))
        )
        return self._worker_runtime(environment).inspect(name)

    def message_worker(
        self,
        name: str,
        text: str,
        *,
        action_id: str | None = None,
        configured_defaults: CodexConfig | None = None,
        model: str | None = None,
        reasoning_effort: str | None = None,
    ) -> WorkerMessageReceipt:
        worker = self._store.get_worker(name)
        if worker is None:
            raise DorfResourceNotFoundError(f"Worker not found: {name}")
        room = self._store.get_current_room(name)
        environment = (
            self._new_environment()
            if room is None
            else self._environment_for_binding(WorkerBinding(worker, room))
        )
        conversation = self._store.get_worker_conversation(name)
        defaults = (
            CodexConfig(conversation.model, conversation.reasoning_effort)
            if conversation is not None
            else configured_defaults or self._agent_defaults
        )
        resolved = resolve_codex_config(
            defaults,
            model=model,
            reasoning_effort=reasoning_effort,
        )
        queued, created = self._worker_runtime(environment).admit_message(
            name,
            message_id=_message_id("wmsg", action_id),
            text=text,
            default_model=resolved.model,
            default_reasoning_effort=resolved.reasoning_effort,
            model=model,
            reasoning_effort=reasoning_effort,
        )
        can_deliver = (
            room is not None and room.status == "ready" and worker.status in {"ready", "assigned"}
        )
        dispatcher_started = (
            launch_worker_message_dispatcher(self._store.database_path, name)
            if can_deliver
            else False
        )
        return WorkerMessageReceipt(queued, created, dispatcher_started)

    def observe_worker_message(
        self,
        name: str,
        *,
        message_id: str | None = None,
    ) -> WorkerWaitResult:
        worker = self._store.get_worker(name)
        if worker is None:
            raise DorfResourceNotFoundError(f"Worker not found: {name}")
        messages = self._store.list_worker_messages(name)
        if not messages:
            raise DorfResourceNotFoundError(f"Worker {name} has no admitted messages")
        target = (
            messages[-1]
            if message_id is None
            else next((message for message in messages if message.id == message_id), None)
        )
        if target is None:
            raise DorfResourceNotFoundError(f"Worker message not found for {name}: {message_id}")
        room = self._store.get_current_room(name)
        environment = (
            self._new_environment()
            if room is None
            else self._environment_for_binding(WorkerBinding(worker, room))
        )
        return self._worker_runtime(environment).observe_wait(name, target.id)

    def wait_for_worker_message(
        self,
        name: str,
        *,
        message_id: str | None = None,
        timeout: float | None = None,
        poll_interval: float = 1.0,
    ) -> WorkerWaitResult:
        started = monotonic()
        while True:
            result = self.observe_worker_message(name, message_id=message_id)
            if result.outcome != "working":
                return result
            if timeout is not None and monotonic() - started >= timeout:
                return result
            sleep(poll_interval)

    def assign_job(
        self,
        name: str,
        *,
        worker_name: str,
        goal: str,
        configured_defaults: CodexConfig | None = None,
        model: str | None = None,
        reasoning_effort: str | None = None,
        activate: bool = True,
    ) -> JobAssignmentResult:
        worker_binding = self._require_worker_binding(worker_name)
        existing = self._store.get_job_binding(name)
        defaults = (
            CodexConfig(
                existing.conversation.model,
                existing.conversation.reasoning_effort,
            )
            if existing is not None
            else configured_defaults or self._agent_defaults
        )
        resolved = resolve_codex_config(
            defaults,
            model=model,
            reasoning_effort=reasoning_effort,
        )
        environment = self._environment_for_binding(worker_binding)
        binding = self._job_runtime(environment).assign(
            NewJob(
                name=name,
                worker_name=worker_name,
                goal=goal,
                model=resolved.model,
                reasoning_effort=resolved.reasoning_effort,
            ),
            activate=activate,
        )
        inputs = self._store.list_job_inputs(name)
        if not inputs:
            raise RuntimeError(f"Job {name} has no initial goal input after assignment")
        initial_input = inputs[0]
        collector_started = (
            launch_assignment_report_collector(
                self._store.database_path,
                binding.job.name,
                binding.assignment.id,
            )
            if activate
            else False
        )
        dispatcher_started = (
            launch_job_input_dispatcher(self._store.database_path, name) if activate else False
        )
        return JobAssignmentResult(
            binding,
            initial_input,
            dispatcher_started,
            collector_started,
        )

    def activate_job(self, name: str) -> JobAssignmentResult:
        execution = self.job_execution(name)
        binding = execution._require_runtime().activate_assignment(name)
        inputs = self._store.list_job_inputs(name)
        if not inputs:
            raise RuntimeError(f"Job {name} has no initial goal input after assignment")
        collector_started = launch_assignment_report_collector(
            self._store.database_path,
            binding.job.name,
            binding.assignment.id,
        )
        dispatcher_started = launch_job_input_dispatcher(self._store.database_path, name)
        return JobAssignmentResult(binding, inputs[0], dispatcher_started, collector_started)

    def message_job(
        self,
        name: str,
        text: str,
        *,
        action_id: str | None = None,
        model: str | None = None,
        reasoning_effort: str | None = None,
    ) -> JobMessageReceipt:
        binding = self._require_job_binding(name)
        resolve_codex_config(
            CodexConfig(
                binding.conversation.model,
                binding.conversation.reasoning_effort,
            ),
            model=model,
            reasoning_effort=reasoning_effort,
        )
        execution = self.job_execution(name)
        queued, created = execution.admit_message(
            message_id=_message_id("jmsg", action_id),
            text=text,
            model=model,
            reasoning_effort=reasoning_effort,
        )
        can_deliver = (
            binding.job.status == "open"
            and binding.assignment.status == "open"
            and binding.worker.current_room_id == binding.assignment.room_id
            and binding.room.status == "ready"
        )
        if can_deliver:
            launch_assignment_report_collector(
                self._store.database_path,
                binding.job.name,
                binding.assignment.id,
            )
        dispatcher_started = (
            launch_job_input_dispatcher(self._store.database_path, name) if can_deliver else False
        )
        return JobMessageReceipt(queued, created, dispatcher_started)

    def end_job(self, name: str, *, interrupt: bool = False) -> JobEndResult:
        binding = self._require_job_binding(name)
        environment = self._environment_for_binding(binding)
        agent = CodexDriver(environment)
        runtime = self._job_runtime(environment, agent)
        if interrupt:
            runtime.recover_turns(name)
        ended = runtime.end(name, interrupt=interrupt)
        dedicated_worker = None
        if ended.worker.lifecycle_policy == "dedicated":
            try:
                dedicated_worker = WorkerRuntime(self._store, environment, agent).end(
                    ended.worker.name,
                    interrupt=interrupt,
                )
            except RuntimeError as error:
                raise DedicatedWorkerCleanupError(ended, error) from error
        return JobEndResult(ended, dedicated_worker)

    def inspect_job(self, name: str) -> JobInspection:
        binding = self._require_job_binding(name)
        return self._job_runtime(self._environment_for_binding(binding)).inspect(name)

    def worker_execution(self, name: str) -> WorkerExecution:
        binding = self._require_worker_binding(name)
        return WorkerExecution(binding, self._environment_for_binding(binding))

    def job_execution(self, name: str) -> JobExecution:
        binding = self._require_job_binding(name)
        environment = self._environment_for_binding(binding)
        agent = CodexDriver(environment)
        return JobExecution(
            binding,
            self._job_runtime(environment, agent),
            environment,
            agent,
            self._git_credential_token,
        )

    @classmethod
    def disposable_job_execution(
        cls,
        binding: JobBinding,
        *,
        environment_config: IncusConfig,
        provider_connection: str,
        provider_gateway: RoomRouteGateway,
        environment_probe: IncusRunnerProbe | None = None,
    ) -> JobExecution:
        environment = new_codex_room_environment(
            environment_config,
            provider_connection,
            gateway=provider_gateway,
            probe=environment_probe,
        )
        environment.install_provider_route(binding)
        agent = CodexDriver(environment)
        return JobExecution(binding, None, environment, agent, None)

    def job_timeline(self, name: str) -> list[TimelineEvent]:
        self._require_job_binding(name)
        return self._store.documents.list_events(name)

    def list_job_artifacts(self, name: str) -> list[JobArtifact]:
        """List path-free retained artifacts for one exact durable Job."""
        job = self._store.get_job(name)
        if job is None:
            raise DorfResourceNotFoundError(f"Job not found: {name}")
        return self._store.documents.list_artifacts(name, job_id=job.id)

    def read_job_artifact(
        self,
        name: str,
        artifact_ref: str,
    ) -> ArtifactReadResult:
        """Read one bounded UTF-8 text/JSON artifact by Job and stable reference."""
        job = self._store.get_job(name)
        if job is None:
            raise DorfResourceNotFoundError(f"Job not found: {name}")
        return self._store.documents.read_artifact(
            name,
            job_id=job.id,
            artifact_ref=artifact_ref,
        )

    def export_job_artifact(
        self,
        name: str,
        artifact_ref: str,
        destination_directory: Path,
        *,
        overwrite: bool = False,
    ) -> ArtifactExportResult:
        """Export exact retained bytes under the artifact's recorded safe filename."""
        job = self._store.get_job(name)
        if job is None:
            raise DorfResourceNotFoundError(f"Job not found: {name}")
        return self._store.documents.export_artifact(
            name,
            job_id=job.id,
            artifact_ref=artifact_ref,
            destination_directory=destination_directory,
            overwrite=overwrite,
        )

    def job_documents_path(self, name: str) -> Path:
        self._require_job_binding(name)
        return self._store.jobs.path(name)

    def observe_job_input(
        self,
        name: str,
        *,
        input_id: str | None = None,
    ) -> AssignedJobWaitResult:
        binding = self._require_job_binding(name)
        inputs = self._store.list_job_inputs(name)
        target = (
            inputs[-1]
            if input_id is None and inputs
            else next((item for item in inputs if item.id == input_id), None)
        )
        if target is None:
            raise DorfResourceNotFoundError(
                f"Job input not found for {name}: {input_id or 'latest'}"
            )
        runtime = self._job_runtime(self._environment_for_binding(binding))
        return runtime.observe_wait(name, target.id)

    def wait_for_job_input(
        self,
        name: str,
        *,
        input_id: str | None = None,
        timeout: float | None = None,
        poll_interval: float = 1.0,
    ) -> AssignedJobWaitResult:
        started = monotonic()
        while True:
            result = self.observe_job_input(name, input_id=input_id)
            if result.outcome != "working":
                return result
            if timeout is not None and monotonic() - started >= timeout:
                return result
            sleep(poll_interval)

    def observe_unsettled_job_input(self, name: str) -> AssignedJobWaitResult | None:
        binding = self._require_job_binding(name)
        inputs = self._store.list_job_inputs(name)
        turns = {turn.input_id: turn for turn in self._store.list_job_turns(name)}
        unsettled = next(
            (
                item
                for item in inputs
                if item.kind != "cleanup"
                and (
                    item.id not in turns
                    or turns[item.id].status in {"running", "recovery-required"}
                )
            ),
            None,
        )
        if unsettled is None:
            return None
        return self._job_runtime(self._environment_for_binding(binding)).observe_wait(
            name, unsettled.id
        )

    def current_job_for_worker(self, name: str) -> str | None:
        return self._store.get_open_job_for_worker(name)

    def _new_environment(self) -> IncusEnvironment | CodexRoomEnvironment:
        if self._provider_connection is None:
            return IncusEnvironment(self._environment_config, probe=self._environment_probe)
        return new_codex_room_environment(
            self._environment_config,
            self._provider_connection,
            gateway=self._gateway_for(self._environment_config),
            probe=self._environment_probe,
        )

    def _environment_for_binding(
        self,
        binding: WorkerBinding | JobBinding,
    ) -> IncusEnvironment | CodexRoomEnvironment:
        if binding.environment_type != IncusEnvironment.environment_type:
            raise UnsupportedRoomTypeError(f"Unsupported Room type: {binding.environment_type}")
        config = IncusConfig.from_mapping(binding.metadata)
        return recorded_codex_room_environment(
            binding,
            gateway=self._gateway_for(config),
            probe=self._environment_probe,
        )

    def _gateway_for(self, config: IncusConfig) -> RoomRouteGateway:
        if self._provider_gateway is None:
            self._provider_gateway = ProviderGateway.open(
                bind_address=incus_bridge_ipv4(config.network)
            )
        return self._provider_gateway

    def _worker_runtime(
        self,
        environment: IncusEnvironment | CodexRoomEnvironment,
    ) -> WorkerRuntime:
        return WorkerRuntime(self._store, environment, CodexDriver(environment))

    def _job_runtime(
        self,
        environment: IncusEnvironment | CodexRoomEnvironment,
        agent: CodexDriver | None = None,
    ) -> JobRuntime:
        return JobRuntime(self._store, environment, agent or CodexDriver(environment))

    def _require_worker_binding(self, name: str) -> WorkerBinding:
        binding = self._store.get_worker_binding(name)
        if binding is not None:
            return binding
        worker = self._store.get_worker(name)
        detail = "not found" if worker is None else "offline with no current Room"
        raise DorfResourceNotFoundError(f"Worker {name} is {detail}")

    def _require_job_binding(self, name: str) -> JobBinding:
        binding = self._store.get_job_binding(name)
        if binding is None:
            raise DorfResourceNotFoundError(f"Job not found: {name}")
        return binding
