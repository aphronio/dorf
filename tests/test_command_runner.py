import os
import signal
import subprocess
import time
from pathlib import Path

import pytest

from dorf.command_runner import run_job_command, shell_command
from dorf.workflows.coding_store import CodingStore


def coding_store(path: Path, job_name: str) -> CodingStore:
    store = CodingStore.open(path)
    store.create_coding_job(job_name=job_name, metadata={})
    return store


def test_timed_out_job_command_records_terminal_workflow_fact(tmp_path: Path) -> None:
    store = coding_store(tmp_path / "state.sqlite3", "timeout-review")

    run = run_job_command(
        store,
        "timeout-review",
        tmp_path,
        shell_command(
            "review:droid",
            "printf 'partial reviewer output\\n'; sleep 5",
            timeout_seconds=1,
        ),
        os.environ,
    )

    assert (run.status, run.exit_code) == ("timed_out", 124)
    assert run.finished_at is not None
    output = Path(run.output_path).read_text()
    assert "partial reviewer output" in output
    assert "Command timed out after 1 seconds." in output


def test_job_command_receives_eof_instead_of_coordinator_stdin(tmp_path: Path) -> None:
    database = tmp_path / "state.sqlite3"
    store = coding_store(database, "noninteractive-review")
    coordinator = subprocess.Popen(
        [
            "python",
            "-c",
            """
import os, sys
from pathlib import Path
from dorf.command_runner import CommandSpec, run_job_command
from dorf.workflows.coding_store import CodingStore
store = CodingStore.open(Path(sys.argv[1]))
run_job_command(
    store,
    'noninteractive-review',
    Path(sys.argv[2]),
    CommandSpec(
        kind='review:codex',
        command=['python', '-c', "import sys; assert sys.stdin.read() == ''; print('stdin-eof')"],
        preview='python stdin EOF probe',
        timeout_seconds=1,
    ),
    os.environ,
)
""",
            str(database),
            str(tmp_path),
        ],
        env=os.environ,
        stdin=subprocess.PIPE,
    )

    assert coordinator.wait(timeout=5) == 0
    assert coordinator.stdin is not None
    coordinator.stdin.close()
    run = store.list_command_runs("noninteractive-review")[0]
    assert (run.status, run.exit_code) == ("succeeded", 0)
    assert Path(run.output_path).read_text() == "stdin-eof\n"


def test_interrupted_job_command_kills_children_and_records_partial_output(tmp_path: Path) -> None:
    database = tmp_path / "state.sqlite3"
    store = coding_store(database, "interrupted-review")
    child_pid_file = tmp_path / "child.pid"
    coordinator = subprocess.Popen(
        [
            "python",
            "-c",
            """
import os, sys
from pathlib import Path
from dorf.command_runner import CommandInterrupted, run_job_command, shell_command
from dorf.workflows.coding_store import CodingStore
store = CodingStore.open(Path(sys.argv[1]))
try:
    run_job_command(store, 'interrupted-review', Path(sys.argv[2]), shell_command(
        'review:droid',
        f\"trap '' TERM; echo $$ > {sys.argv[3]}; echo streamed; while :; do sleep 1; done\"
    ), os.environ)
except CommandInterrupted:
    pass
""",
            str(database),
            str(tmp_path),
            str(child_pid_file),
        ],
        env=os.environ,
    )
    for _ in range(100):
        if child_pid_file.exists():
            break
        time.sleep(0.02)
    assert child_pid_file.exists()
    child_pid = int(child_pid_file.read_text())

    coordinator.send_signal(signal.SIGTERM)
    assert coordinator.wait(timeout=10) == 0

    run = store.list_command_runs("interrupted-review")[0]
    assert (run.status, run.exit_code) == ("interrupted", 130)
    assert run.finished_at is not None
    assert "streamed" in Path(run.output_path).read_text()
    with pytest.raises(ProcessLookupError):
        os.kill(child_pid, 0)
