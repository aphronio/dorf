import json
import shutil
import subprocess
from dataclasses import replace
from pathlib import Path

import pytest

from dorf.report_collector import (
    StaleAssignmentError,
    _environment_for_assignment,
    collect_assignment_reports_once,
)
from dorf.runtime import JobRuntime, NewJob, NewWorker, RuntimeStore, WorkerRuntime


class AssignmentRoom:
    def __init__(self, root: Path, job_name: str) -> None:
        self.root = root
        self.prefix = f"/run/dorf/jobs/{job_name}/outbox/"
        self.fail_next_ack = False
        self.pull_failure: Exception | None = None

    def _path(self, room_path: str) -> Path:
        assert room_path.startswith(self.prefix)
        return self.root / room_path.removeprefix(self.prefix)

    def execute(self, binding, argv, *, input=None, **kwargs):
        if argv[:2] == ["find", f"{self.prefix}new"]:
            output = "".join(f"{path.name}\n" for path in sorted((self.root / "new").iterdir()))
            return subprocess.CompletedProcess(argv, 0, output, "")
        if argv[0] == "mkdir":
            for value in argv[2:]:
                self._path(value).mkdir(parents=True, exist_ok=True)
            return subprocess.CompletedProcess(argv, 0, "", "")
        if argv[0] == "tee":
            if self.fail_next_ack and "/acks/" in argv[1]:
                self.fail_next_ack = False
                return subprocess.CompletedProcess(argv, 1, "", "collector interrupted")
            path = self._path(argv[1])
            path.parent.mkdir(parents=True, exist_ok=True)
            path.write_text(input or "")
            return subprocess.CompletedProcess(argv, 0, input or "", "")
        if argv[0] == "mv":
            destination = self._path(argv[2])
            destination.parent.mkdir(parents=True, exist_ok=True)
            self._path(argv[1]).replace(destination)
            return subprocess.CompletedProcess(argv, 0, "", "")
        raise AssertionError(argv)

    def pull_file(self, binding, room_path: str, destination: Path, *, max_bytes: int):
        if self.pull_failure is not None:
            error = self.pull_failure
            self.pull_failure = None
            raise error
        source = self._path(room_path)
        if source.is_symlink() or not source.is_file():
            raise ValueError("Room artifact must be a regular file")
        if source.stat().st_size > max_bytes:
            raise ValueError("Room artifact exceeds transfer limit")
        destination.parent.mkdir(parents=True, exist_ok=True)
        shutil.copyfile(source, destination)


class ReadyEnvironment:
    environment_type = "incus-vm"
    workspace = "/workspace"

    def environment_id(self, worker_name: str) -> str:
        return f"dorf-{worker_name}"

    def initial_metadata(self, worker_name: str) -> dict[str, str]:
        return {"template": "dorf-codex"}

    def create(self, binding) -> None:
        pass

    def execute(self, binding, argv, **kwargs):
        return subprocess.CompletedProcess(argv, 0, "", "")

    def stop(self, binding):
        return "stopped"

    def destroy(self, binding):
        return "deleted"


class ReadyAgent:
    agent_type = "codex"

    def prepare(self, binding) -> None:
        pass


def assigned_job(tmp_path: Path):
    store = RuntimeStore.open(tmp_path / "state.sqlite3")
    environment = ReadyEnvironment()
    agent = ReadyAgent()
    WorkerRuntime(store, environment, agent).spawn(NewWorker("researcher"))
    binding = JobRuntime(store, environment, agent).assign(
        NewJob(
            "checkout-perf",
            "researcher",
            "Make checkout instant",
            "gpt-5.6-sol",
            "high",
        )
    )
    return store, binding


def test_collector_reconstructs_recorded_codex_room_facade(tmp_path, monkeypatch) -> None:
    _, binding = assigned_job(tmp_path)
    routed_environment = object()
    seen = []

    def reconstruct(current_binding):
        seen.append(current_binding)
        return routed_environment

    monkeypatch.setattr(
        "dorf.report_collector.recorded_codex_room_environment",
        reconstruct,
    )

    assert _environment_for_assignment(binding) is routed_environment
    assert seen == [binding]


def test_stale_collector_scope_does_not_touch_the_current_assignment_outbox(
    tmp_path,
) -> None:
    store, binding = assigned_job(tmp_path)
    room_root = tmp_path / "room-outbox"
    bundle = room_root / "new" / "report-current"
    bundle.mkdir(parents=True)
    (bundle / "manifest.json").write_text("{}")
    stale = replace(
        binding,
        assignment=replace(binding.assignment, id="assignment-stale"),
    )

    with pytest.raises(StaleAssignmentError, match="no longer current"):
        collect_assignment_reports_once(
            store,
            AssignmentRoom(room_root, "checkout-perf"),
            stale,
        )

    assert (bundle / "manifest.json").read_text() == "{}"
    assert not (room_root / "acks").exists()
    assert all(event.source != "worker" for event in store.documents.list_events("checkout-perf"))


def test_mismatched_assignment_report_is_rejected_without_timeline_attribution(
    tmp_path,
) -> None:
    store, binding = assigned_job(tmp_path)
    room_root = tmp_path / "room-outbox"
    bundle = room_root / "new" / "report-stale"
    bundle.mkdir(parents=True)
    (bundle / "manifest.json").write_text(
        json.dumps(
            {
                "schema_version": 1,
                "id": "report-stale",
                "job": "checkout-perf",
                "assignment": "assignment-stale",
                "kind": "progress",
                "summary": "Claim from a stale Assignment",
                "artifacts": [],
            }
        )
    )

    accepted = collect_assignment_reports_once(
        store,
        AssignmentRoom(room_root, "checkout-perf"),
        binding,
    )

    assert accepted == []
    assert all(event.source != "worker" for event in store.documents.list_events("checkout-perf"))
    acknowledgement = json.loads((room_root / "acks" / "report-stale.json").read_text())
    assert acknowledgement["status"] == "rejected"
    assert "Assignment does not match" in acknowledgement["detail"]
    assert (room_root / "cur" / "report-stale").is_dir()


def test_assignment_collector_recovers_acknowledgement_without_duplicate_event(
    tmp_path,
) -> None:
    store, binding = assigned_job(tmp_path)
    room_root = tmp_path / "room-outbox"
    bundle = room_root / "new" / "report-progress"
    bundle.mkdir(parents=True)
    (bundle / "manifest.json").write_text(
        json.dumps(
            {
                "schema_version": 1,
                "id": "report-progress",
                "job": "checkout-perf",
                "assignment": binding.assignment.id,
                "kind": "progress",
                "summary": "Found the slow query",
                "artifacts": [],
            }
        )
    )
    room = AssignmentRoom(room_root, "checkout-perf")
    room.fail_next_ack = True

    with pytest.raises(RuntimeError, match="collector interrupted"):
        collect_assignment_reports_once(store, room, binding)
    retried = collect_assignment_reports_once(store, room, binding)

    assert retried == []
    assert [
        event.id
        for event in store.documents.list_events("checkout-perf")
        if event.source == "worker"
    ] == ["report-progress"]
    acknowledgement = json.loads((room_root / "acks" / "report-progress.json").read_text())
    assert acknowledgement["status"] == "accepted"


def test_transient_assignment_room_failure_leaves_report_for_retry(tmp_path) -> None:
    store, binding = assigned_job(tmp_path)
    room_root = tmp_path / "room-outbox"
    bundle = room_root / "new" / "report-progress"
    bundle.mkdir(parents=True)
    (bundle / "manifest.json").write_text(
        json.dumps(
            {
                "schema_version": 1,
                "id": "report-progress",
                "job": "checkout-perf",
                "assignment": binding.assignment.id,
                "kind": "progress",
                "summary": "Found the slow query",
                "artifacts": [],
            }
        )
    )
    room = AssignmentRoom(room_root, "checkout-perf")
    room.pull_failure = RuntimeError("Room temporarily unavailable")

    with pytest.raises(RuntimeError, match="temporarily unavailable"):
        collect_assignment_reports_once(store, room, binding)

    assert bundle.is_dir()
    assert not (room_root / "acks" / "report-progress.json").exists()
    assert [event.id for event in collect_assignment_reports_once(store, room, binding)] == [
        "report-progress"
    ]


def test_assignment_collector_rejects_linked_artifact(tmp_path) -> None:
    store, binding = assigned_job(tmp_path)
    room_root = tmp_path / "room-outbox"
    bundle = room_root / "new" / "report-link"
    (bundle / "files").mkdir(parents=True)
    outside = tmp_path / "outside.txt"
    outside.write_text("secret")
    (bundle / "files" / "0001").symlink_to(outside)
    (bundle / "manifest.json").write_text(
        json.dumps(
            {
                "schema_version": 1,
                "id": "report-link",
                "job": "checkout-perf",
                "assignment": binding.assignment.id,
                "kind": "evidence",
                "summary": "Linked artifact",
                "artifacts": [
                    {
                        "file": "0001",
                        "name": "outside.txt",
                        "media_type": "text/plain",
                    }
                ],
            }
        )
    )

    accepted = collect_assignment_reports_once(
        store, AssignmentRoom(room_root, "checkout-perf"), binding
    )

    assert accepted == []
    assert all(event.id != "report-link" for event in store.documents.list_events("checkout-perf"))
    acknowledgement = json.loads((room_root / "acks" / "report-link.json").read_text())
    assert acknowledgement["status"] == "rejected"
    assert "regular file" in acknowledgement["detail"]


def test_assignment_collector_rejects_artifact_over_streaming_quota(tmp_path, monkeypatch) -> None:
    store, binding = assigned_job(tmp_path)
    room_root = tmp_path / "room-outbox"
    bundle = room_root / "new" / "report-large"
    (bundle / "files").mkdir(parents=True)
    (bundle / "files" / "0001").write_bytes(b"1234")
    (bundle / "manifest.json").write_text(
        json.dumps(
            {
                "schema_version": 1,
                "id": "report-large",
                "job": "checkout-perf",
                "assignment": binding.assignment.id,
                "kind": "evidence",
                "summary": "Large artifact",
                "artifacts": [
                    {
                        "file": "0001",
                        "name": "large.bin",
                        "media_type": "application/octet-stream",
                    }
                ],
            }
        )
    )
    monkeypatch.setattr("dorf.report_collector.MAX_ARTIFACT_BYTES", 3)

    accepted = collect_assignment_reports_once(
        store, AssignmentRoom(room_root, "checkout-perf"), binding
    )

    assert accepted == []
    acknowledgement = json.loads((room_root / "acks" / "report-large.json").read_text())
    assert acknowledgement["status"] == "rejected"
    assert "transfer limit" in acknowledgement["detail"]


def test_assignment_report_is_accepted_with_exact_identity_provenance(tmp_path) -> None:
    store, binding = assigned_job(tmp_path)
    room_root = tmp_path / "room-outbox"
    bundle = room_root / "new" / "report-profile"
    (bundle / "files").mkdir(parents=True)
    (bundle / "files" / "0001").write_text("p95=120ms\n")
    (bundle / "manifest.json").write_text(
        json.dumps(
            {
                "schema_version": 1,
                "id": "report-profile",
                "job": "checkout-perf",
                "assignment": binding.assignment.id,
                "kind": "evidence",
                "summary": "Captured checkout profile",
                "artifacts": [
                    {
                        "file": "0001",
                        "name": "profile.txt",
                        "media_type": "text/plain",
                    }
                ],
            }
        )
    )

    accepted = collect_assignment_reports_once(
        store,
        AssignmentRoom(room_root, "checkout-perf"),
        binding,
    )

    assert [event.id for event in accepted] == ["report-profile"]
    event = next(
        event
        for event in store.documents.list_events("checkout-perf")
        if event.id == "report-profile"
    )
    assert event.related == {
        "assignment": binding.assignment.id,
        "conversation": binding.conversation.id,
        "room": binding.room.id,
        "worker": "researcher",
    }
    assert event.artifacts[0].digest.startswith("sha256:")
    acknowledgement = json.loads((room_root / "acks" / "report-profile.json").read_text())
    assert acknowledgement["status"] == "accepted"
