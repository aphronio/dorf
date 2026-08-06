from __future__ import annotations

import os
import shlex
import signal
import subprocess
import threading
from collections.abc import Mapping, Sequence
from contextlib import contextmanager
from dataclasses import dataclass
from pathlib import Path
from typing import Protocol

import typer


@dataclass(frozen=True)
class CommandSpec:
    kind: str
    command: str | Sequence[str]
    preview: str
    shell: bool = False
    timeout_seconds: int | None = None
    requires_provider_route: bool = False


class CommandRun(Protocol):
    id: int
    job_name: str
    kind: str
    command: str
    status: str


class CommandStore(Protocol):
    database_path: Path

    def create_command_run(
        self, job_name: str, kind: str, command: str, output_path: str
    ) -> CommandRun: ...

    def set_command_run_output_path(self, run_id: int, output_path: str) -> CommandRun: ...

    def finish_command_run(self, run_id: int, status: str, exit_code: int) -> CommandRun: ...


class CommandInterrupted(RuntimeError):
    """A workflow command was interrupted after its run was finalized."""

    def __init__(self, run: CommandRun) -> None:
        super().__init__(f"{run.kind} was interrupted")
        self.run = run


def argv_command(kind: str, argv: Sequence[str]) -> CommandSpec:
    return CommandSpec(kind=kind, command=list(argv), preview=shlex.join(argv))


def shell_command(
    kind: str,
    command: str,
    *,
    timeout_seconds: int | None = None,
    requires_provider_route: bool = False,
) -> CommandSpec:
    return CommandSpec(
        kind=kind,
        command=command,
        preview=command,
        shell=True,
        timeout_seconds=timeout_seconds,
        requires_provider_route=requires_provider_route,
    )


def run_job_command(
    store: CommandStore,
    job_name: str,
    workspace_path: Path,
    spec: CommandSpec,
    env: Mapping[str, str],
    prepared_run: CommandRun | None = None,
) -> CommandRun:
    run = prepared_run or store.create_command_run(
        job_name=job_name,
        kind=spec.kind,
        command=spec.preview,
        output_path="",
    )
    if (
        run.job_name != job_name
        or run.kind != spec.kind
        or run.command != spec.preview
        or run.status != "running"
    ):
        raise RuntimeError("Prepared command run does not match the requested command")
    output_path = (
        store.database_path.parent / "runs" / "coding" / job_name / str(run.id) / "output.log"
    )
    output_path.parent.mkdir(parents=True, exist_ok=True)
    run = store.set_command_run_output_path(run.id, str(output_path))

    exit_code, status = _run_process(workspace_path, spec, dict(env), output_path)
    finished = store.finish_command_run(run.id, status, exit_code)
    if status == "interrupted":
        raise CommandInterrupted(finished)
    return finished


def _run_process(
    workspace_path: Path,
    spec: CommandSpec,
    env: dict[str, str],
    output_path: Path,
) -> tuple[int, str]:
    with output_path.open("w") as output:
        previous_handlers: dict[signal.Signals, object] = {}
        try:
            process = subprocess.Popen(
                spec.command,
                cwd=workspace_path,
                env=env,
                shell=spec.shell,
                stdin=subprocess.DEVNULL,
                stdout=subprocess.PIPE,
                stderr=subprocess.STDOUT,
                text=True,
                start_new_session=True,
            )
        except FileNotFoundError:
            message = f"Command not found: {spec.command[0]}\n"
            output.write(message)
            output.flush()
            typer.echo(message, nl=False)
            return 127, "failed"
        except OSError as error:
            message = f"Could not launch command: {spec.preview}: {error}\n"
            output.write(message)
            output.flush()
            typer.echo(message, nl=False)
            return 126, "failed"

        try:
            with _interrupt_on_termination(previous_handlers):
                return _wait_for_process(process, spec, output)
        except KeyboardInterrupt:
            _terminate_process_group(process)
            return 130, "interrupted"
        except BaseException:
            _terminate_process_group(process)
            raise


def _wait_for_process(
    process: subprocess.Popen,
    spec: CommandSpec,
    output,
) -> tuple[int, str]:
    output_thread = None
    try:
        if spec.timeout_seconds is not None:
            output_thread = threading.Thread(
                target=_stream_process_output,
                args=(process, output),
                daemon=True,
            )
            output_thread.start()
            try:
                exit_code = process.wait(timeout=spec.timeout_seconds)
            except subprocess.TimeoutExpired:
                _terminate_process_group(process)
                output_thread.join()
                message = f"Command timed out after {spec.timeout_seconds} seconds.\n"
                output.write(message)
                output.flush()
                typer.echo(message, nl=False)
                return 124, "timed_out"
            output_thread.join()
            return exit_code, "succeeded" if exit_code == 0 else "failed"
        if process.stdout is not None:
            for chunk in process.stdout:
                output.write(chunk)
                output.flush()
                typer.echo(chunk, nl=False)
        exit_code = process.wait()
        return exit_code, "succeeded" if exit_code == 0 else "failed"
    except BaseException:
        _terminate_process_group(process)
        if output_thread is not None:
            output_thread.join()
        raise


def _terminate_process_group(process: subprocess.Popen) -> None:
    if process.poll() is not None:
        process.wait()
        return
    _signal_process_group(process, signal.SIGTERM)
    try:
        process.wait(timeout=5)
    except subprocess.TimeoutExpired:
        _signal_process_group(process, signal.SIGKILL)
        process.wait()


@contextmanager
def _interrupt_on_termination(previous_handlers: dict[signal.Signals, object]):
    signals = (signal.SIGHUP, signal.SIGINT, signal.SIGTERM)
    try:
        for signum in signals:
            previous_handlers[signum] = signal.signal(signum, _raise_interrupt)
        yield
    finally:
        for signum, handler in previous_handlers.items():
            signal.signal(signum, handler)


def _raise_interrupt(signum: int, frame: object) -> None:
    raise KeyboardInterrupt


def _signal_process_group(process: subprocess.Popen, sig: signal.Signals) -> None:
    try:
        os.killpg(process.pid, sig)
    except ProcessLookupError:
        pass


def _stream_process_output(process: subprocess.Popen, output) -> None:
    if process.stdout is None:
        return
    for chunk in process.stdout:
        output.write(chunk)
        output.flush()
        typer.echo(chunk, nl=False)
