"""Human- and agent-readable Job timeline and evidence documents."""

from __future__ import annotations

import fcntl
import hashlib
import json
import os
import re
import secrets
import stat
from collections.abc import Iterator
from contextlib import contextmanager
from dataclasses import asdict, dataclass, field
from datetime import UTC, datetime
from pathlib import Path
from typing import Any, Literal

from .job import JobDirectory

TIMELINE_SCHEMA_VERSION = 1
MAX_ARTIFACT_BYTES = 100 * 1024 * 1024
MAX_JOB_ARTIFACT_BYTES = 500 * 1024 * 1024
MAX_ARTIFACTS_PER_EVENT = 20
MAX_MODEL_ARTIFACT_BYTES = 64 * 1024
_EVENT_ID = re.compile(r"^[a-z][a-z0-9-]{0,127}$")
_ARTIFACT_REF = re.compile(r"^artifact-v1-([1-9][0-9]*)-([0-9a-f]{64})$")
_SOURCES = {"client", "runtime", "worker", "workflow"}
_PROVENANCE = {"claim", "fact"}


@dataclass(frozen=True)
class ArtifactInput:
    name: str
    path: Path
    media_type: str = "application/octet-stream"


@dataclass(frozen=True)
class ArtifactRecord:
    name: str
    digest: str
    size: int
    media_type: str
    path: str


@dataclass(frozen=True)
class JobArtifact:
    """Path-free identity and provenance for one retained Job artifact."""

    ref: str
    job_id: int
    job_name: str
    event_id: str
    event_sequence: int
    source: str
    provenance: str
    assignment_id: str | None
    name: str
    digest: str
    size: int
    media_type: str


ArtifactReadStatus = Literal[
    "ok",
    "missing",
    "cross-job",
    "unsupported-media",
    "oversized",
    "corrupt",
    "invalid-encoding",
    "invalid-json",
]


@dataclass(frozen=True)
class ArtifactReadResult:
    """A bounded model-readable artifact outcome with no storage-path detail."""

    status: ArtifactReadStatus
    artifact: JobArtifact | None = None
    content: str | None = None


ArtifactExportStatus = Literal[
    "ok",
    "missing",
    "cross-job",
    "corrupt",
    "destination-exists",
]


@dataclass(frozen=True)
class ArtifactExportResult:
    """An exact retained-artifact export outcome with no storage-path detail."""

    status: ArtifactExportStatus
    artifact: JobArtifact | None = None
    destination: Path | None = None


@dataclass(frozen=True)
class TimelineEvent:
    schema_version: int
    id: str
    job: str
    sequence: int
    recorded_at: str
    source: str
    provenance: str
    kind: str
    summary: str
    related: dict[str, str] = field(default_factory=dict)
    artifacts: tuple[ArtifactRecord, ...] = ()


class JobDocumentStore:
    """Append immutable, ordinary-file documents to named Jobs."""

    def __init__(self, jobs: JobDirectory) -> None:
        self.jobs = jobs

    def append_event(
        self,
        job_name: str,
        *,
        event_id: str,
        source: str,
        provenance: str,
        kind: str,
        summary: str,
        related: dict[str, str] | None = None,
        artifacts: list[ArtifactInput] | None = None,
    ) -> tuple[TimelineEvent, bool]:
        self._validate_event(event_id, source, provenance, kind, summary)
        job_path = self.jobs.path(job_name)
        if job_path.is_symlink() or not job_path.is_dir():
            raise RuntimeError(f"Job not found: {job_name}")
        artifact_inputs = artifacts or []
        if len(artifact_inputs) > MAX_ARTIFACTS_PER_EVENT:
            raise ValueError("Timeline event has too many artifacts")
        with self._timeline_lock(job_name):
            stored_artifacts = tuple(
                self._store_artifact(job_name, artifact) for artifact in artifact_inputs
            )
            expected = {
                "source": source,
                "provenance": provenance,
                "kind": kind,
                "summary": summary,
                "related": related or {},
                "artifacts": stored_artifacts,
            }
            events = self.list_events(job_name)
            existing = next((event for event in events if event.id == event_id), None)
            if existing is not None:
                actual = {
                    "source": existing.source,
                    "provenance": existing.provenance,
                    "kind": existing.kind,
                    "summary": existing.summary,
                    "related": existing.related,
                    "artifacts": existing.artifacts,
                }
                if actual != expected:
                    raise ValueError("Timeline event ID is already bound to different content")
                return existing, False
            event = TimelineEvent(
                schema_version=TIMELINE_SCHEMA_VERSION,
                id=event_id,
                job=job_name,
                sequence=events[-1].sequence + 1 if events else 1,
                recorded_at=datetime.now(UTC).isoformat(timespec="microseconds"),
                source=source,
                provenance=provenance,
                kind=kind,
                summary=summary,
                related=related or {},
                artifacts=stored_artifacts,
            )
            timeline_path = job_path / "timeline"
            timeline_path.mkdir(mode=0o700, exist_ok=True)
            _write_json_atomically(
                timeline_path / f"{event.sequence:06d}-{event.id}.json",
                _event_payload(event),
            )
            return event, True

    def list_events(self, job_name: str) -> list[TimelineEvent]:
        job_path = self.jobs.path(job_name)
        if job_path.is_symlink() or not job_path.is_dir():
            raise RuntimeError(f"Job not found: {job_name}")
        timeline_path = job_path / "timeline"
        if timeline_path.is_symlink():
            raise RuntimeError(f"Job timeline path must not be a symbolic link: {job_name}")
        if not timeline_path.exists():
            return []
        events = [_read_event(path, job_name) for path in sorted(timeline_path.glob("*.json"))]
        if [event.sequence for event in events] != list(range(1, len(events) + 1)):
            raise RuntimeError(f"Job timeline sequence is invalid: {job_name}")
        return events

    def list_artifacts(self, job_name: str, *, job_id: int) -> list[JobArtifact]:
        """List path-free retained artifact metadata in timeline order."""
        return [
            self._manifest_entry(job_name, job_id, event, index, artifact)
            for event in self.list_events(job_name)
            for index, artifact in enumerate(event.artifacts)
        ]

    def read_artifact(
        self,
        job_name: str,
        *,
        job_id: int,
        artifact_ref: str,
        max_bytes: int = MAX_MODEL_ARTIFACT_BYTES,
    ) -> ArtifactReadResult:
        """Read one exact supported artifact after verifying retained-byte custody."""
        if max_bytes < 1 or max_bytes > MAX_MODEL_ARTIFACT_BYTES:
            raise ValueError(
                f"Artifact read limit must be between 1 and {MAX_MODEL_ARTIFACT_BYTES} bytes"
            )
        parsed_ref = _ARTIFACT_REF.fullmatch(artifact_ref)
        if parsed_ref is not None and int(parsed_ref.group(1)) != job_id:
            return ArtifactReadResult("cross-job")

        selected: tuple[JobArtifact, ArtifactRecord] | None = None
        for event in self.list_events(job_name):
            for index, artifact in enumerate(event.artifacts):
                manifest = self._manifest_entry(job_name, job_id, event, index, artifact)
                if manifest.ref == artifact_ref:
                    selected = manifest, artifact
                    break
            if selected is not None:
                break
        if selected is None:
            return ArtifactReadResult("missing")

        manifest, record = selected
        if not _is_supported_model_media(record.media_type):
            return ArtifactReadResult("unsupported-media", manifest)
        if record.size > max_bytes:
            return ArtifactReadResult("oversized", manifest)

        expected_digest = _digest_hex(record.digest)
        if expected_digest is None:
            return ArtifactReadResult("corrupt", manifest)
        expected_relative = f"artifacts/sha256/{expected_digest[:2]}/{expected_digest}"
        if record.path != expected_relative:
            return ArtifactReadResult("corrupt", manifest)
        path = self.jobs.path(job_name) / expected_relative
        descriptor = -1
        try:
            descriptor = os.open(path, os.O_RDONLY | getattr(os, "O_NOFOLLOW", 0))
            metadata = os.fstat(descriptor)
            if not stat.S_ISREG(metadata.st_mode) or metadata.st_size != record.size:
                os.close(descriptor)
                descriptor = -1
                return ArtifactReadResult("corrupt", manifest)
        except OSError:
            if descriptor >= 0:
                os.close(descriptor)
            return ArtifactReadResult("corrupt", manifest)

        try:
            with os.fdopen(descriptor, "rb") as retained:
                descriptor = -1
                content_bytes = retained.read(max_bytes + 1)
        except OSError:
            return ArtifactReadResult("corrupt", manifest)
        finally:
            if descriptor >= 0:
                os.close(descriptor)

        if len(content_bytes) != record.size or len(content_bytes) > max_bytes:
            return ArtifactReadResult("corrupt", manifest)
        if hashlib.sha256(content_bytes).hexdigest() != expected_digest:
            return ArtifactReadResult("corrupt", manifest)
        try:
            content = content_bytes.decode("utf-8")
        except UnicodeDecodeError:
            return ArtifactReadResult("invalid-encoding", manifest)
        if _is_json_media(record.media_type):
            try:
                json.loads(content)
            except json.JSONDecodeError:
                return ArtifactReadResult("invalid-json", manifest)
        return ArtifactReadResult("ok", manifest, content)

    def export_artifact(
        self,
        job_name: str,
        *,
        job_id: int,
        artifact_ref: str,
        destination_directory: Path,
        overwrite: bool = False,
    ) -> ArtifactExportResult:
        """Atomically copy one verified retained artifact to a caller-owned directory."""
        try:
            destination_metadata = destination_directory.lstat()
        except OSError as error:
            raise ValueError("Artifact export destination must be an existing directory") from error
        if destination_directory.is_symlink() or not stat.S_ISDIR(destination_metadata.st_mode):
            raise ValueError("Artifact export destination must be an existing directory")

        parsed_ref = _ARTIFACT_REF.fullmatch(artifact_ref)
        if parsed_ref is not None and int(parsed_ref.group(1)) != job_id:
            return ArtifactExportResult("cross-job")

        selected: tuple[JobArtifact, ArtifactRecord] | None = None
        for event in self.list_events(job_name):
            for index, artifact in enumerate(event.artifacts):
                manifest = self._manifest_entry(job_name, job_id, event, index, artifact)
                if manifest.ref == artifact_ref:
                    selected = manifest, artifact
                    break
            if selected is not None:
                break
        if selected is None:
            return ArtifactExportResult("missing")

        manifest, record = selected
        destination = destination_directory / record.name
        if (
            not record.name
            or record.name in {".", ".."}
            or "/" in record.name
            or "\\" in record.name
            or len(record.name) > 255
        ):
            return ArtifactExportResult("corrupt", manifest)
        if not overwrite and (destination.exists() or destination.is_symlink()):
            return ArtifactExportResult("destination-exists", manifest, destination)

        expected_digest = _digest_hex(record.digest)
        if expected_digest is None:
            return ArtifactExportResult("corrupt", manifest)
        expected_relative = f"artifacts/sha256/{expected_digest[:2]}/{expected_digest}"
        if record.path != expected_relative:
            return ArtifactExportResult("corrupt", manifest)
        retained_path = self.jobs.path(job_name) / expected_relative
        source_descriptor = -1
        temporary_path = destination_directory / (
            f".{record.name}.{secrets.token_hex(8)}.dorf-export.tmp"
        )
        try:
            source_descriptor = os.open(
                retained_path,
                os.O_RDONLY | getattr(os, "O_NOFOLLOW", 0),
            )
            metadata = os.fstat(source_descriptor)
            if not stat.S_ISREG(metadata.st_mode) or metadata.st_size != record.size:
                os.close(source_descriptor)
                source_descriptor = -1
                return ArtifactExportResult("corrupt", manifest)
        except OSError:
            if source_descriptor >= 0:
                os.close(source_descriptor)
            return ArtifactExportResult("corrupt", manifest)

        copied_digest = hashlib.sha256()
        copied_size = 0
        try:
            with (
                os.fdopen(source_descriptor, "rb") as retained,
                temporary_path.open("xb") as output,
            ):
                source_descriptor = -1
                os.chmod(temporary_path, 0o600)
                while True:
                    try:
                        chunk = retained.read(1024 * 1024)
                    except OSError:
                        return ArtifactExportResult("corrupt", manifest)
                    if not chunk:
                        break
                    copied_size += len(chunk)
                    copied_digest.update(chunk)
                    output.write(chunk)
                output.flush()
                os.fsync(output.fileno())
            if copied_size != record.size or copied_digest.hexdigest() != expected_digest:
                return ArtifactExportResult("corrupt", manifest)
            if overwrite:
                os.replace(temporary_path, destination)
            else:
                try:
                    os.link(temporary_path, destination, follow_symlinks=False)
                except FileExistsError:
                    return ArtifactExportResult("destination-exists", manifest, destination)
                temporary_path.unlink()
            return ArtifactExportResult("ok", manifest, destination)
        finally:
            if source_descriptor >= 0:
                os.close(source_descriptor)
            temporary_path.unlink(missing_ok=True)

    @staticmethod
    def _manifest_entry(
        job_name: str,
        job_id: int,
        event: TimelineEvent,
        index: int,
        artifact: ArtifactRecord,
    ) -> JobArtifact:
        identity = "\0".join(
            (
                str(job_id),
                job_name,
                event.id,
                str(index),
                artifact.digest,
            )
        )
        ref = f"artifact-v1-{job_id}-{hashlib.sha256(identity.encode()).hexdigest()}"
        return JobArtifact(
            ref=ref,
            job_id=job_id,
            job_name=job_name,
            event_id=event.id,
            event_sequence=event.sequence,
            source=event.source,
            provenance=event.provenance,
            assignment_id=event.related.get("assignment"),
            name=artifact.name,
            digest=artifact.digest,
            size=artifact.size,
            media_type=artifact.media_type,
        )

    def _store_artifact(self, job_name: str, artifact: ArtifactInput) -> ArtifactRecord:
        if (
            not artifact.name
            or artifact.name in {".", ".."}
            or "/" in artifact.name
            or "\\" in artifact.name
            or len(artifact.name) > 255
        ):
            raise ValueError("Artifact name is invalid")
        if not artifact.media_type or len(artifact.media_type) > 255:
            raise ValueError("Artifact media type is invalid")
        try:
            metadata = artifact.path.lstat()
        except OSError as error:
            raise ValueError(f"Artifact is unreadable: {artifact.name}: {error}") from error
        if not stat.S_ISREG(metadata.st_mode):
            raise ValueError(f"Artifact must be a regular file: {artifact.name}")
        descriptor = os.open(
            artifact.path,
            os.O_RDONLY | getattr(os, "O_NOFOLLOW", 0),
        )
        temporary_path: Path | None = None
        try:
            artifacts_root = self.jobs.path(job_name) / "artifacts" / "sha256"
            artifacts_root.mkdir(parents=True, exist_ok=True, mode=0o700)
            temporary_path = artifacts_root / f".artifact.{secrets.token_hex(8)}.tmp"
            digest = hashlib.sha256()
            size = 0
            with os.fdopen(descriptor, "rb") as source, temporary_path.open("xb") as output:
                descriptor = -1
                os.chmod(temporary_path, 0o600)
                while chunk := source.read(1024 * 1024):
                    size += len(chunk)
                    if size > MAX_ARTIFACT_BYTES:
                        raise ValueError(f"Artifact exceeds 100 MiB limit: {artifact.name}")
                    digest.update(chunk)
                    output.write(chunk)
                output.flush()
                os.fsync(output.fileno())
            hexdigest = digest.hexdigest()
            destination = artifacts_root / hexdigest[:2] / hexdigest
            if not destination.exists():
                used = sum(
                    path.stat().st_size
                    for path in artifacts_root.glob("[0-9a-f][0-9a-f]/*")
                    if path.is_file() and not path.is_symlink()
                )
                if used + size > MAX_JOB_ARTIFACT_BYTES:
                    raise ValueError("Job artifacts exceed 500 MiB limit")
                destination.parent.mkdir(mode=0o700, exist_ok=True)
                temporary_path.replace(destination)
                temporary_path = None
            relative_path = destination.relative_to(self.jobs.path(job_name)).as_posix()
            return ArtifactRecord(
                name=artifact.name,
                digest=f"sha256:{hexdigest}",
                size=size,
                media_type=artifact.media_type,
                path=relative_path,
            )
        finally:
            if descriptor >= 0:
                os.close(descriptor)
            if temporary_path is not None and temporary_path.exists():
                temporary_path.unlink()

    @staticmethod
    def _validate_event(
        event_id: str, source: str, provenance: str, kind: str, summary: str
    ) -> None:
        if not _EVENT_ID.fullmatch(event_id):
            raise ValueError("Timeline event ID is invalid")
        if source not in _SOURCES:
            raise ValueError(f"Timeline event source is invalid: {source}")
        if provenance not in _PROVENANCE:
            raise ValueError(f"Timeline event provenance is invalid: {provenance}")
        if not _EVENT_ID.fullmatch(kind):
            raise ValueError("Timeline event kind is invalid")
        if (
            not summary.strip()
            or summary != summary.strip()
            or len(summary) > 4096
            or any(character in summary for character in ("\n", "\r", "\x00"))
        ):
            raise ValueError("Timeline event summary must be one line containing 1-4096 characters")

    @contextmanager
    def _timeline_lock(self, job_name: str) -> Iterator[None]:
        descriptor = os.open(
            self.jobs.path(job_name) / ".timeline.lock",
            os.O_RDWR | os.O_CREAT | getattr(os, "O_NOFOLLOW", 0),
            0o600,
        )
        try:
            os.fchmod(descriptor, 0o600)
            fcntl.flock(descriptor, fcntl.LOCK_EX)
            yield
        finally:
            fcntl.flock(descriptor, fcntl.LOCK_UN)
            os.close(descriptor)


def _event_payload(event: TimelineEvent) -> dict[str, Any]:
    payload = asdict(event)
    payload["artifacts"] = [asdict(artifact) for artifact in event.artifacts]
    return payload


def _read_event(path: Path, job_name: str) -> TimelineEvent:
    if path.is_symlink():
        raise RuntimeError(f"Job timeline entry must not be a symbolic link: {path.name}")
    try:
        payload = json.loads(path.read_text())
        artifacts = tuple(ArtifactRecord(**item) for item in payload.pop("artifacts"))
        event = TimelineEvent(**payload, artifacts=artifacts)
    except (OSError, ValueError, TypeError, KeyError, json.JSONDecodeError) as error:
        raise RuntimeError(f"Job timeline entry is unreadable: {path.name}: {error}") from error
    if (
        event.schema_version != TIMELINE_SCHEMA_VERSION
        or event.job != job_name
        or path.name != f"{event.sequence:06d}-{event.id}.json"
    ):
        raise RuntimeError(f"Job timeline entry is invalid: {path.name}")
    return event


def _write_json_atomically(path: Path, payload: dict[str, Any]) -> None:
    temporary = path.parent / f".{path.name}.{secrets.token_hex(8)}.tmp"
    descriptor = os.open(
        temporary,
        os.O_WRONLY | os.O_CREAT | os.O_EXCL | getattr(os, "O_NOFOLLOW", 0),
        0o600,
    )
    try:
        with os.fdopen(descriptor, "w") as output:
            json.dump(payload, output, indent=2, sort_keys=True, ensure_ascii=False)
            output.write("\n")
            output.flush()
            os.fsync(output.fileno())
        temporary.replace(path)
        directory = os.open(path.parent, os.O_RDONLY)
        try:
            os.fsync(directory)
        finally:
            os.close(directory)
    finally:
        if temporary.exists():
            temporary.unlink()


def _media_type(value: str) -> str:
    return value.partition(";")[0].strip().lower()


def _is_json_media(value: str) -> bool:
    media_type = _media_type(value)
    return media_type == "application/json" or media_type.endswith("+json")


def _is_supported_model_media(value: str) -> bool:
    media_type = _media_type(value)
    return media_type.startswith("text/") or _is_json_media(value)


def _digest_hex(value: str) -> str | None:
    if not value.startswith("sha256:"):
        return None
    hexdigest = value.removeprefix("sha256:")
    if len(hexdigest) != 64 or any(character not in "0123456789abcdef" for character in hexdigest):
        return None
    return hexdigest
