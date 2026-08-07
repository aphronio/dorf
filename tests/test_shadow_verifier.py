import signal
import subprocess
from pathlib import Path
from types import SimpleNamespace

import pytest

from dorf.workflows.coding import CommandInterrupted
from dorf.workflows.coding_store import CodingStore
from dorf.workflows.shadow_verifier import run_shadow_review


class Execution:
    def __init__(self, fail=False):
        self.calls = []
        self.fail = fail

    def execute(self, argv, **kwargs):
        self.calls.append((argv, kwargs))
        code = 1 if self.fail and len(self.calls) == 1 else 0
        return subprocess.CompletedProcess(argv, code, "", "")

    def process_command(self, argv, **kwargs):
        self.calls.append((argv, kwargs))
        return ["bash", "-lc", "printf DORF_REVIEW_NO_FINDINGS"]


class FakeDorf:
    def __init__(self, execution):
        self.execution = execution
        self.ended = []

    def spawn_worker(self, name, **kwargs):
        self.name = name
        return SimpleNamespace(room=SimpleNamespace(id="room-1"))

    def worker_execution(self, name):
        assert name == self.name
        return self.execution

    def end_worker(self, name, *, interrupt=False):
        self.ended.append((name, interrupt))


class Gateway:
    def route_for_consumer(self, consumer):
        assert consumer == "room:room-1"
        return SimpleNamespace(base_url="http://gateway/v1")


class GitHub:
    def get_branch_sha(self, repo, branch):
        assert (repo, branch) == ("owner/repo", "dorf/job")
        return "b" * 40


def job(store):
    return store.create_coding_job(
        job_name="job",
        status="active",
        metadata={
            "github_repo": "owner/repo",
            "job_branch": "dorf/job",
            "target_start_sha": "a" * 40,
        },
    )


def test_shadow_review_is_read_only_retained_and_cleaned(tmp_path: Path) -> None:
    store = CodingStore.open(tmp_path / "state.sqlite3")
    execution = Execution()
    dorf = FakeDorf(execution)

    run = run_shadow_review(
        store, job(store), dorf, Gateway(), GitHub(), "super-secret-value"
    )

    assert Path(run.output_path).read_text() == "DORF_REVIEW_NO_FINDINGS"
    assert run.git_commit_before == run.git_commit_after == "b" * 40
    assert dorf.ended == [(dorf.name, True)]
    rendered = " ".join(str(argv) for argv, _ in execution.calls)
    assert "--tools read,grep,find,ls" in rendered
    assert "deepseek-v4-flash" in rendered
    assert "super-secret-value" not in rendered
    assert execution.calls[-1][1]["provider_route"] is True


def test_shadow_review_cleans_up_when_clone_fails(tmp_path: Path) -> None:
    store = CodingStore.open(tmp_path / "state.sqlite3")
    dorf = FakeDorf(Execution(fail=True))

    with pytest.raises(RuntimeError, match="clone"):
        run_shadow_review(
            store, job(store), dorf, Gateway(), GitHub(), "super-secret-value"
        )

    assert dorf.ended == [(dorf.name, True)]


def test_shadow_review_persists_interrupted_run_and_translates_keyboard_interrupt(
    tmp_path: Path,
    monkeypatch,
) -> None:
    store = CodingStore.open(tmp_path / "state.sqlite3")
    dorf = FakeDorf(Execution())

    class InterruptedProcess:
        pid = 1234
        returncode = None

        def poll(self):
            return self.returncode

        def wait(self, timeout=None):
            if timeout == 1800:
                raise KeyboardInterrupt
            self.returncode = -15
            return self.returncode

    process = InterruptedProcess()
    stopped = []
    monkeypatch.setattr(subprocess, "Popen", lambda *args, **kwargs: process)
    monkeypatch.setattr(
        "dorf.workflows.shadow_verifier.os.killpg",
        lambda pid, sig: stopped.append((pid, sig)),
    )

    with pytest.raises(CommandInterrupted) as raised:
        run_shadow_review(
            store, job(store), dorf, Gateway(), GitHub(), "super-secret-value"
        )

    runs = store.list_command_runs("job")
    assert len(runs) == 1
    assert raised.value.run == runs[0]
    assert (runs[0].status, runs[0].exit_code) == ("interrupted", 130)
    assert stopped == [(process.pid, signal.SIGTERM)]
    assert dorf.ended == [(dorf.name, True)]
