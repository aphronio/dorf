"""Replaceable local dispatcher for durable Worker-general message FIFOs."""

from __future__ import annotations

import argparse
import subprocess
import sys
from pathlib import Path

from dorf.adapters.agents.codex import CodexDriver
from dorf.codex_room import recorded_codex_room_environment
from dorf.runtime import (
    RuntimeStore,
    WorkerMessage,
    WorkerOfflineError,
    WorkerRuntime,
)


def launch_worker_message_dispatcher(database_path: Path, worker_name: str) -> bool:
    """Start a detached, replaceable Worker-message dispatcher."""
    try:
        subprocess.Popen(
            [
                sys.executable,
                "-m",
                "dorf.worker_message_dispatcher",
                "--database",
                str(database_path),
                "--worker",
                worker_name,
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


def dispatch_worker_messages(database_path: Path, worker_name: str) -> None:
    """Deliver direct messages in order until the FIFO is empty or blocked."""
    store = RuntimeStore.open(database_path)
    with store.worker_message_dispatcher_lock(worker_name):
        while True:
            target = _next_message(store, worker_name)
            if target is None:
                return
            binding = store.get_worker_binding(worker_name)
            if (
                binding is None
                or binding.worker.status not in {"ready", "assigned"}
                or binding.room.status != "ready"
            ):
                return
            runtime = _runtime_for_message(store, worker_name)
            try:
                outcome = runtime.deliver_message(worker_name, target.id)
            except WorkerOfflineError:
                return
            if outcome.status == "succeeded":
                continue
            if outcome.status in {"running", "recovery-required"}:
                return
            return


def _runtime_for_message(store: RuntimeStore, worker_name: str) -> WorkerRuntime:
    binding = store.get_worker_binding(worker_name)
    if binding is None:
        raise RuntimeError(f"Worker not found: {worker_name}")
    environment = recorded_codex_room_environment(binding)
    return WorkerRuntime(store, environment, CodexDriver(environment))


def _next_message(store: RuntimeStore, worker_name: str) -> WorkerMessage | None:
    for message in store.list_worker_messages(worker_name):
        turn = store.get_worker_turn_by_message(worker_name, message.id)
        if turn is None or turn.status in {"running", "recovery-required"}:
            return message
        if turn.status != "succeeded":
            return None
    return None


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument("--database", type=Path, required=True)
    parser.add_argument("--worker", required=True)
    args = parser.parse_args()
    dispatch_worker_messages(args.database, args.worker)


if __name__ == "__main__":
    main()
