"""Replaceable local dispatcher for durable Job input FIFOs."""

from __future__ import annotations

import argparse
import subprocess
import sys
from pathlib import Path
from typing import TYPE_CHECKING

from dorf.runtime import JobInput, RuntimeStore, WorkerOfflineError

if TYPE_CHECKING:
    from dorf.sdk import JobExecution


def launch_job_input_dispatcher(database_path: Path, job_name: str) -> bool:
    """Start a detached, replaceable Job-input dispatcher."""
    try:
        subprocess.Popen(
            [
                sys.executable,
                "-m",
                "dorf.job_input_dispatcher",
                "--database",
                str(database_path),
                "--job",
                job_name,
            ],
            stdin=subprocess.DEVNULL,
            stdout=subprocess.DEVNULL,
            stderr=subprocess.DEVNULL,
            close_fds=True,
            start_new_session=True,
        )
    except OSError:
        return False
    return True


def dispatch_job_inputs(database_path: Path, job_name: str) -> None:
    """Deliver Job inputs in order until the FIFO is empty or blocked."""
    store = RuntimeStore.open(database_path)
    with store.job_input_dispatcher_lock(job_name):
        while True:
            target = _next_input(store, job_name)
            if target is None:
                return
            binding = store.get_job_binding(job_name)
            if (
                binding is None
                or binding.job.status != "open"
                or binding.assignment.status != "open"
                or binding.worker.current_room_id != binding.assignment.room_id
                or binding.room.status != "ready"
            ):
                return
            execution = _execution_for_input(store, job_name)
            try:
                outcome = execution.deliver_input(target.id)
            except WorkerOfflineError:
                return
            if outcome.status == "succeeded":
                continue
            if outcome.status in {"running", "recovery-required"}:
                return
            return


def _execution_for_input(store: RuntimeStore, job_name: str) -> JobExecution:
    from dorf.sdk import Dorf

    return Dorf(store).job_execution(job_name)


def _next_input(store: RuntimeStore, job_name: str) -> JobInput | None:
    for item in store.list_job_inputs(job_name):
        turn = store.get_job_turn_by_input(job_name, item.id)
        if turn is None or turn.status in {"running", "recovery-required"}:
            return item
        if turn.status != "succeeded":
            return None
    return None


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument("--database", type=Path, required=True)
    parser.add_argument("--job", required=True)
    args = parser.parse_args()
    dispatch_job_inputs(args.database, args.job)


if __name__ == "__main__":
    main()
