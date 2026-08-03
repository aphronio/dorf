"""Tangible durable Job goal and initial Assignment documents."""

from __future__ import annotations

import json
import os
import re
import secrets
import shutil
from dataclasses import dataclass
from datetime import UTC, datetime
from pathlib import Path
from typing import Any

_JOB_NAME = re.compile(r"^[a-z0-9][a-z0-9-]{0,62}$")
JOB_SCHEMA_VERSION = 2


class InvalidJobNameError(ValueError):
    pass


@dataclass(frozen=True)
class JobRecord:
    schema_version: int
    name: str
    goal: dict[str, Any]
    assignment: dict[str, Any]
    created_at: str


class JobDirectory:
    """Store immutable human-readable Job goal and provenance in named directories."""

    def __init__(self, root: Path) -> None:
        self.root = root

    def path(self, name: str) -> Path:
        validate_job_name(name)
        return self.root / name

    def create_assigned(
        self,
        *,
        name: str,
        goal: str,
        worker_name: str,
        room_id: str,
        workspace: str,
        assignment_id: str,
        assignment_generation: int,
    ) -> tuple[JobRecord, bool]:
        """Atomically publish complete goal v1 and initial Assignment provenance."""
        validate_job_name(name)
        if not goal.strip():
            raise ValueError("Job goal cannot be empty")
        self.root.mkdir(parents=True, exist_ok=True, mode=0o700)
        final = self.path(name)
        if final.is_symlink():
            raise RuntimeError(f"Job path must not be a symbolic link: {name}")
        expected_assignment = {
            "id": assignment_id,
            "generation": assignment_generation,
            "worker": worker_name,
            "room": room_id,
            "workspace": workspace,
        }
        if final.exists():
            record = self.get(name)
            if record is None:
                raise RuntimeError(f"Job directory exists without a record: {name}")
            if (
                record.goal.get("version") != 1
                or record.goal.get("text") != goal
                or record.assignment != expected_assignment
            ):
                raise RuntimeError(f"Job {name} already has different goal or provenance")
            return record, False

        now = _now()
        record = JobRecord(
            schema_version=JOB_SCHEMA_VERSION,
            name=name,
            goal={"version": 1, "text": goal, "assigned_at": now},
            assignment=expected_assignment,
            created_at=now,
        )
        temporary = self.root / f".{name}.{secrets.token_hex(8)}.tmp"
        temporary.mkdir(mode=0o700)
        try:
            self._write_record(temporary, record)
            _write_private_text(temporary / "goal.md", f"# Goal\n\n{goal}\n")
            try:
                temporary.rename(final)
                _fsync_directory(self.root)
            except FileExistsError:
                return self.create_assigned(
                    name=name,
                    goal=goal,
                    worker_name=worker_name,
                    room_id=room_id,
                    workspace=workspace,
                    assignment_id=assignment_id,
                    assignment_generation=assignment_generation,
                )
        finally:
            if temporary.exists():
                shutil.rmtree(temporary)
        return record, True

    def get(self, name: str) -> JobRecord | None:
        path = self.path(name)
        if path.is_symlink():
            raise RuntimeError(f"Job path must not be a symbolic link: {name}")
        if not path.is_dir():
            return None
        try:
            payload = json.loads((path / "job.json").read_text())
        except (OSError, json.JSONDecodeError) as error:
            raise RuntimeError(f"Job record is unreadable: {name}: {error}") from error
        return _record_from_payload(payload, expected_name=name)

    @staticmethod
    def _write_record(path: Path, record: JobRecord) -> None:
        payload = {
            "schema_version": record.schema_version,
            "name": record.name,
            "goal": record.goal,
            "assignment": record.assignment,
            "created_at": record.created_at,
        }
        _write_private_text(
            path / "job.json",
            json.dumps(payload, indent=2, sort_keys=True, ensure_ascii=False) + "\n",
        )


def validate_job_name(name: str) -> None:
    if not _JOB_NAME.fullmatch(name):
        raise InvalidJobNameError(
            "Job name must be 1-63 lowercase letters, numbers, or hyphens, "
            "starting with a letter or number"
        )


def _record_from_payload(payload: Any, *, expected_name: str) -> JobRecord:
    if not isinstance(payload, dict):
        raise RuntimeError(f"Job record must be an object: {expected_name}")
    if payload.get("schema_version") != JOB_SCHEMA_VERSION:
        raise RuntimeError(f"Job record has an unsupported schema: {expected_name}")
    if payload.get("name") != expected_name:
        raise RuntimeError(f"Job record name does not match its directory: {expected_name}")
    if not isinstance(payload.get("goal"), dict) or not isinstance(payload.get("assignment"), dict):
        raise RuntimeError(f"Job record has invalid goal or Assignment provenance: {expected_name}")
    if not isinstance(payload.get("created_at"), str):
        raise RuntimeError(f"Job record has invalid creation data: {expected_name}")
    return JobRecord(
        schema_version=payload["schema_version"],
        name=payload["name"],
        goal=payload["goal"],
        assignment=payload["assignment"],
        created_at=payload["created_at"],
    )


def _write_private_text(path: Path, value: str) -> None:
    temporary = path.with_name(f".{path.name}.{secrets.token_hex(8)}.tmp")
    descriptor = os.open(
        temporary,
        os.O_WRONLY | os.O_CREAT | os.O_EXCL | getattr(os, "O_NOFOLLOW", 0),
        0o600,
    )
    try:
        with os.fdopen(descriptor, "w") as output:
            descriptor = -1
            output.write(value)
            output.flush()
            os.fsync(output.fileno())
        os.replace(temporary, path)
        _fsync_directory(path.parent)
    finally:
        if descriptor >= 0:
            os.close(descriptor)
        temporary.unlink(missing_ok=True)


def _fsync_directory(path: Path) -> None:
    descriptor = os.open(path, os.O_RDONLY | getattr(os, "O_DIRECTORY", 0))
    try:
        os.fsync(descriptor)
    finally:
        os.close(descriptor)


def _now() -> str:
    return datetime.now(UTC).isoformat(timespec="microseconds")
