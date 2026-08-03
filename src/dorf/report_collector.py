"""Replaceable controller for validated Worker report ingestion."""

from __future__ import annotations

import argparse
import json
import re
import secrets
import shutil
import subprocess
import sys
import time
from pathlib import Path
from typing import Protocol

from dorf.codex_room import (
    CodexRoomEnvironment,
    recorded_codex_room_environment,
)
from dorf.runtime import (
    ArtifactInput,
    JobBinding,
    RuntimeStore,
    TimelineEvent,
)
from dorf.runtime.documents import MAX_ARTIFACT_BYTES

_REPORT_ID = re.compile(r"^report-[a-z0-9][a-z0-9-]{0,120}$")
_REPORT_KINDS = {"progress", "assumption", "evidence", "completed"}
_MANIFEST_LIMIT = 64 * 1024


class StaleAssignmentError(RuntimeError):
    """The collector no longer owns the current Job Assignment."""


class ReportRoom(Protocol):
    def execute(
        self,
        binding: JobBinding,
        argv: list[str],
        *,
        input: str | None = None,
        **kwargs,
    ): ...

    def pull_file(
        self,
        binding: JobBinding,
        room_path: str,
        destination: Path,
        *,
        max_bytes: int,
    ) -> None: ...


def launch_assignment_report_collector(
    database_path: Path, job_name: str, assignment_id: str
) -> bool:
    """Start a replaceable collector fenced to one Assignment."""
    try:
        subprocess.Popen(
            [
                sys.executable,
                "-m",
                "dorf.report_collector",
                "--database",
                str(database_path),
                "--job",
                job_name,
                "--assignment",
                assignment_id,
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


def collect_assignment_reports(
    database_path: Path,
    job_name: str,
    assignment_id: str,
    *,
    poll_interval: float = 1.0,
) -> None:
    store = RuntimeStore.open(database_path)
    with store.assignment_report_collector_lock(job_name, assignment_id) as acquired:
        if not acquired:
            return
        while True:
            binding = store.get_job_binding(job_name)
            if (
                binding is None
                or binding.job.status != "open"
                or binding.assignment.status != "open"
                or binding.assignment.id != assignment_id
            ):
                return
            environment = _environment_for_assignment(binding)
            try:
                collect_assignment_reports_once(store, environment, binding)
            except StaleAssignmentError:
                return
            time.sleep(poll_interval)


def _environment_for_assignment(binding: JobBinding) -> CodexRoomEnvironment:
    return recorded_codex_room_environment(binding)


def collect_assignment_reports_once(
    store: RuntimeStore,
    room: ReportRoom,
    binding: JobBinding,
) -> list[TimelineEvent]:
    """Ingest reports fenced to one current independent Assignment."""
    _require_current_assignment(store, binding)
    return _collect_reports_once(
        store,
        room,
        binding,
        outbox=f"/run/dorf/jobs/{binding.job.name}/outbox",
        assignment_id=binding.assignment.id,
        related={
            "assignment": binding.assignment.id,
            "conversation": binding.conversation.id,
            "room": binding.room.id,
            "worker": binding.worker.name,
        },
    )


def _collect_reports_once(
    store: RuntimeStore,
    room: ReportRoom,
    binding: JobBinding,
    *,
    outbox: str,
    assignment_id: str,
    related: dict[str, str],
) -> list[TimelineEvent]:
    listing = room.execute(
        binding,
        [
            "find",
            f"{outbox}/new",
            "-mindepth",
            "1",
            "-maxdepth",
            "1",
            "-type",
            "d",
            "-printf",
            "%f\\n",
        ],
    )
    if listing.returncode != 0:
        raise RuntimeError((listing.stderr or "Could not list Worker reports").strip())
    report_ids = [
        value for value in listing.stdout.splitlines() if value and _REPORT_ID.fullmatch(value)
    ]
    accepted: list[TimelineEvent] = []
    for report_id in sorted(report_ids):
        try:
            event, created = _ingest_report(
                store,
                room,
                binding,
                report_id,
                outbox=outbox,
                assignment_id=assignment_id,
                related=related,
            )
        except ValueError as error:
            _reject(room, binding, report_id, str(error), outbox=outbox)
            continue
        if created:
            accepted.append(event)
    return accepted


def _ingest_report(
    store: RuntimeStore,
    room: ReportRoom,
    binding: JobBinding,
    report_id: str,
    *,
    outbox: str,
    assignment_id: str,
    related: dict[str, str],
) -> tuple[TimelineEvent, bool]:
    _require_current_assignment(store, binding)
    quarantine = (
        store.database_path.parent
        / "quarantine"
        / binding.job.name
        / assignment_id
        / f"{report_id}-{secrets.token_hex(8)}"
    )
    quarantine.mkdir(parents=True, mode=0o700)
    try:
        manifest_path = quarantine / "manifest.json"
        room.pull_file(
            binding,
            f"{outbox}/new/{report_id}/manifest.json",
            manifest_path,
            max_bytes=_MANIFEST_LIMIT,
        )
        manifest = _validated_manifest(
            manifest_path,
            binding.job.name,
            report_id,
            assignment_id=assignment_id,
        )
        artifacts: list[ArtifactInput] = []
        for item in manifest["artifacts"]:
            local_path = quarantine / item["file"]
            room.pull_file(
                binding,
                f"{outbox}/new/{report_id}/files/{item['file']}",
                local_path,
                max_bytes=MAX_ARTIFACT_BYTES,
            )
            artifacts.append(ArtifactInput(item["name"], local_path, item["media_type"]))
        _require_current_assignment(store, binding)
        event, created = store.documents.append_event(
            binding.job.name,
            event_id=report_id,
            source="worker",
            provenance="claim",
            kind=manifest["kind"],
            summary=manifest["summary"],
            related=related,
            artifacts=artifacts,
        )
        _acknowledge(room, binding, report_id, event, outbox=outbox)
        return event, created
    finally:
        shutil.rmtree(quarantine, ignore_errors=True)


def _validated_manifest(
    path: Path,
    job_name: str,
    report_id: str,
    *,
    assignment_id: str,
) -> dict:
    try:
        payload = json.loads(path.read_text())
    except (OSError, json.JSONDecodeError) as error:
        raise ValueError(f"Worker report manifest is unreadable: {error}") from error
    if not isinstance(payload, dict) or payload.get("schema_version") != 1:
        raise ValueError("Worker report schema is unsupported")
    expected_fields = {
        "artifacts",
        "id",
        "job",
        "kind",
        "schema_version",
        "summary",
    }
    expected_fields.add("assignment")
    actual_fields = set(payload)
    if actual_fields != expected_fields:
        raise ValueError("Worker report fields are invalid")
    if payload.get("id") != report_id or payload.get("job") != job_name:
        raise ValueError("Worker report identity does not match its Room binding")
    if payload.get("assignment") != assignment_id:
        raise ValueError("Worker report Assignment does not match its Room binding")
    kind = payload.get("kind")
    summary = payload.get("summary")
    items = payload.get("artifacts")
    if kind not in _REPORT_KINDS:
        raise ValueError("Worker report kind is invalid")
    if (
        not isinstance(summary, str)
        or not summary.strip()
        or summary != summary.strip()
        or len(summary) > 4096
        or any(character in summary for character in ("\n", "\r", "\x00"))
    ):
        raise ValueError("Worker report summary is invalid")
    if not isinstance(items, list) or len(items) > 20:
        raise ValueError("Worker report artifact list is invalid")
    expected_files = [f"{index:04d}" for index in range(1, len(items) + 1)]
    actual_files: list[str] = []
    for item in items:
        if not isinstance(item, dict):
            raise ValueError("Worker report artifact is invalid")
        if set(item) != {"file", "media_type", "name"}:
            raise ValueError("Worker report artifact fields are invalid")
        file_name = item.get("file")
        name = item.get("name")
        media_type = item.get("media_type")
        if not all(isinstance(value, str) for value in (file_name, name, media_type)):
            raise ValueError("Worker report artifact fields are invalid")
        if (
            not name
            or name in {".", ".."}
            or "/" in name
            or "\\" in name
            or len(name) > 255
            or not media_type
            or len(media_type) > 255
        ):
            raise ValueError("Worker report artifact metadata is invalid")
        actual_files.append(file_name)
    if actual_files != expected_files:
        raise ValueError("Worker report artifact paths are invalid")
    return payload


def _reject(
    room: ReportRoom,
    binding: JobBinding,
    report_id: str,
    detail: str,
    *,
    outbox: str,
) -> None:
    _execute_checked(room, binding, ["mkdir", "-p", f"{outbox}/cur"])
    acknowledgement = (
        json.dumps(
            {"detail": detail[:1000], "status": "rejected"},
            indent=2,
            sort_keys=True,
        )
        + "\n"
    )
    _execute_checked(
        room,
        binding,
        ["tee", f"{outbox}/acks/{report_id}.json"],
        input=acknowledgement,
    )
    _execute_checked(
        room,
        binding,
        ["mv", f"{outbox}/new/{report_id}", f"{outbox}/cur/{report_id}"],
    )


def _acknowledge(
    room: ReportRoom,
    binding: JobBinding,
    report_id: str,
    event: TimelineEvent,
    *,
    outbox: str,
) -> None:
    _execute_checked(room, binding, ["mkdir", "-p", f"{outbox}/cur"])
    acknowledgement = (
        json.dumps(
            {
                "event_id": event.id,
                "sequence": event.sequence,
                "status": "accepted",
            },
            indent=2,
            sort_keys=True,
        )
        + "\n"
    )
    _execute_checked(
        room,
        binding,
        ["tee", f"{outbox}/acks/{report_id}.json"],
        input=acknowledgement,
    )
    _execute_checked(
        room,
        binding,
        ["mv", f"{outbox}/new/{report_id}", f"{outbox}/cur/{report_id}"],
    )


def _execute_checked(
    room: ReportRoom,
    binding: JobBinding,
    argv: list[str],
    *,
    input: str | None = None,
) -> None:
    result = room.execute(binding, argv, input=input)
    if result.returncode != 0:
        raise RuntimeError((result.stderr or result.stdout or "Room command failed").strip())


def _require_current_assignment(store: RuntimeStore, expected: JobBinding) -> None:
    current = store.get_job_binding(expected.job.name)
    if (
        current is None
        or current.job.status != "open"
        or current.assignment.status != "open"
        or current.assignment.id != expected.assignment.id
        or current.assignment.worker_name != expected.worker.name
        or current.assignment.room_id != expected.room.id
        or current.worker.current_room_id != expected.room.id
    ):
        raise StaleAssignmentError(f"Assignment is no longer current: {expected.assignment.id}")


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument("--database", type=Path, required=True)
    parser.add_argument("--job", required=True)
    parser.add_argument("--assignment", required=True)
    args = parser.parse_args()
    collect_assignment_reports(args.database, args.job, args.assignment)


if __name__ == "__main__":
    main()
