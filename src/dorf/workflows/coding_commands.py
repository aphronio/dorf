"""Repository command execution owned by the coding workflow."""

from __future__ import annotations

import os
import subprocess
from collections.abc import Mapping
from pathlib import Path
from typing import Protocol

from dorf.command_runner import CommandInterrupted, CommandSpec, run_job_command, shell_command
from dorf.repo_contract import RepoContract
from dorf.runtime import ArtifactInput, JobBinding

from .coding_store import CodingCommandRun, CodingJob, CodingStore

GIT_PROBE_TIMEOUT_SECONDS = 10


class CodingEnvironment(Protocol):
    def execute(
        self,
        binding: JobBinding,
        argv: list[str],
        *,
        cwd: str | None = None,
        env: Mapping[str, str] | None = None,
        input: str | None = None,
        timeout_seconds: float | None = None,
    ) -> subprocess.CompletedProcess[str]: ...

    def process_command(
        self,
        binding: JobBinding,
        argv: list[str],
        *,
        cwd: str | None = None,
        env: Mapping[str, str] | None = None,
        provider_route: bool = False,
    ) -> list[str]: ...


def run_coding_job_command(
    store: CodingStore,
    environment: CodingEnvironment,
    job: CodingJob,
    binding: JobBinding,
    contract: RepoContract,
    spec: CommandSpec,
) -> CodingCommandRun:
    before = _git_head(environment, binding)
    interruption = None
    argv = ["bash", "-lc", str(spec.command)] if spec.shell else list(spec.command)
    process_spec = CommandSpec(
        kind=spec.kind,
        command=environment.process_command(
            binding,
            argv,
            cwd=binding.workspace,
            env=_command_env(job, binding, contract),
            provider_route=spec.requires_provider_route,
        ),
        preview=spec.preview,
        timeout_seconds=spec.timeout_seconds,
    )
    try:
        run = run_job_command(
            store=store,
            job_name=job.job_name,
            workspace_path=store.database_path.parent,
            spec=process_spec,
            env=os.environ.copy(),
        )
    except CommandInterrupted as error:
        run = error.run
        interruption = error
    after = _git_head(environment, binding)
    recorded = store.set_command_run_git_commits(run.id, before=before, after=after)
    output_path = Path(recorded.output_path)
    if output_path.is_file():
        artifact_name = f"{recorded.kind.replace('/', '-')}-output.log"
        store.documents.append_event(
            job.job_name,
            event_id=f"evt-command-{recorded.id}-output",
            source="workflow",
            provenance="fact",
            kind="command-result",
            summary=f"{recorded.kind} {recorded.status} (exit {recorded.exit_code})",
            related={
                "assignment": binding.assignment.id,
                "run": str(recorded.id),
                "room": binding.room.id,
                "worker": binding.worker.name,
            },
            artifacts=[ArtifactInput(artifact_name, output_path, "text/plain")],
        )
    if interruption is not None:
        raise CommandInterrupted(recorded) from None
    return recorded


def prepare_coding_repository(
    store: CodingStore,
    environment: CodingEnvironment,
    job: CodingJob,
    binding: JobBinding,
    contract: RepoContract,
) -> CodingCommandRun | None:
    """Run the repository's deterministic preparation before its first agent turn."""
    command = contract.commands.get("prepare")
    if command is None:
        return None
    return run_coding_job_command(
        store,
        environment,
        job,
        binding,
        contract,
        shell_command("prepare", command),
    )


def _command_env(
    job: CodingJob,
    binding: JobBinding,
    contract: RepoContract,
) -> dict[str, str]:
    env = {
        "DORF_JOB_NAME": job.job_name,
        "DORF_WORKER_NAME": binding.worker.name,
        "DORF_ASSIGNMENT_ID": binding.assignment.id,
        "DORF_WORKSPACE": binding.workspace,
        "DORF_TARGET_BRANCH": job.target_branch,
        "DORF_TARGET_START_SHA": job.target_start_sha,
        "DORF_JOB_BRANCH": job.job_branch,
        "DORF_READY_PATH": "/tmp/dorf/ready.json",
    }
    sources = {
        "dorf.job_name": job.job_name,
        "dorf.worker_name": binding.worker.name,
        "dorf.assignment_id": binding.assignment.id,
        "dorf.workspace": binding.workspace,
        "dorf.target_repo": job.target_repo,
        "dorf.target_branch": job.target_branch,
        "dorf.target_start_sha": job.target_start_sha,
        "dorf.job_branch": job.job_branch,
        "dorf.ready_path": "/tmp/dorf/ready.json",
    }
    for name, source in contract.env.items():
        if source.startswith("host."):
            host_name = source.removeprefix("host.")
            if host_name in os.environ:
                env[name] = os.environ[host_name]
            continue
        if source in sources:
            env[name] = sources[source]
            continue
        raise ValueError(f"Unsupported env source for {name}: {source}")
    return env


def _git_head(
    environment: CodingEnvironment,
    binding: JobBinding,
) -> str | None:
    try:
        result = environment.execute(
            binding,
            ["git", "rev-parse", "HEAD"],
            cwd=binding.workspace,
            timeout_seconds=GIT_PROBE_TIMEOUT_SECONDS,
        )
    except (OSError, subprocess.TimeoutExpired):
        return None
    return result.stdout.strip() if result.returncode == 0 else None
