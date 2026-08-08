"""Coding-to-PR state keyed by independent Job identity."""

from __future__ import annotations

import hashlib
import json
import sqlite3
from dataclasses import asdict, dataclass
from datetime import UTC, datetime

from dorf.runtime import RuntimeStore


@dataclass(frozen=True)
class CodingJob:
    job_name: str
    status: str
    metadata: dict[str, str]
    github_pr_number: int | None
    github_pr_url: str | None
    created_at: str
    updated_at: str

    @property
    def task(self) -> str:
        return self.metadata.get("task", "")

    @property
    def target_repo(self) -> str:
        return self.metadata.get("target_repo", "")

    @property
    def target_branch(self) -> str:
        return self.metadata.get("target_branch", "")

    @property
    def target_start_sha(self) -> str:
        return self.metadata.get("target_start_sha", "")

    @property
    def job_branch(self) -> str:
        return self.metadata.get("job_branch", "")


@dataclass(frozen=True)
class CodingCommandRun:
    id: int
    job_name: str
    kind: str
    command: str
    status: str
    exit_code: int | None
    started_at: str
    finished_at: str | None
    output_path: str
    git_commit_before: str | None
    git_commit_after: str | None

    @property
    def command_name(self) -> str:
        return self.kind


@dataclass(frozen=True)
class AcceptanceItem:
    key: str
    text: str
    source: str
    verifier: str
    verifier_ref: str
    verifier_command: str = ""


@dataclass(frozen=True)
class AcceptanceChecklist:
    job_name: str
    goal_digest: str
    items: tuple[AcceptanceItem, ...]
    state: str
    revision: int
    created_at: str
    updated_at: str


class CodingStore(RuntimeStore):
    """Keep workflow policy separate from runtime resource authority."""

    def create_coding_job(
        self,
        *,
        job_name: str,
        metadata: dict[str, str],
        status: str = "setting-up",
    ) -> CodingJob:
        now = _now()
        try:
            self._connection.execute("BEGIN IMMEDIATE")
            self._connection.execute(
                """
                INSERT INTO coding_jobs (
                    job_name, status, metadata,
                    github_pr_number, github_pr_url, created_at, updated_at
                ) VALUES (?, ?, ?, NULL, NULL, ?, ?)
                """,
                (job_name, status, json.dumps(metadata, sort_keys=True), now, now),
            )
            self._connection.commit()
        except BaseException:
            self._connection.rollback()
            raise
        created = self.get_coding_job(job_name)
        if created is None:
            raise RuntimeError("created coding Job could not be loaded")
        return created

    def create_coding_job_with_acceptance(
        self,
        *,
        job_name: str,
        metadata: dict[str, str],
        goal: str,
        items: tuple[AcceptanceItem, ...],
        status: str = "setting-up",
    ) -> CodingJob:
        """Atomically reserve a coding Job and acceptance checklist."""
        _validate_acceptance_items(items)
        now = _now()
        goal_digest = f"sha256:{hashlib.sha256(goal.encode()).hexdigest()}"
        encoded = json.dumps([asdict(item) for item in items], sort_keys=True)
        try:
            self._connection.execute("BEGIN IMMEDIATE")
            self._connection.execute(
                """
                INSERT INTO coding_jobs (
                    job_name, status, metadata,
                    github_pr_number, github_pr_url, created_at, updated_at
                ) VALUES (?, ?, ?, NULL, NULL, ?, ?)
                """,
                (job_name, status, json.dumps(metadata, sort_keys=True), now, now),
            )
            self._connection.execute(
                """
                INSERT INTO coding_acceptance_checklists (
                    job_name, goal_digest, items, state, revision, created_at, updated_at
                ) VALUES (?, ?, ?, 'draft', 1, ?, ?)
                """,
                (job_name, goal_digest, encoded, now, now),
            )
            self._connection.commit()
        except BaseException:
            self._connection.rollback()
            raise
        created = self.get_coding_job(job_name)
        if created is None:
            raise RuntimeError("created coding Job could not be loaded")
        return created

    def get_coding_job(self, job_name: str) -> CodingJob | None:
        row = self._connection.execute(
            "SELECT * FROM coding_jobs WHERE job_name = ?", (job_name,)
        ).fetchone()
        return _coding_job(row) if row is not None else None

    def list_coding_jobs(self) -> list[CodingJob]:
        rows = self._connection.execute(
            "SELECT * FROM coding_jobs ORDER BY created_at DESC, job_name DESC"
        ).fetchall()
        return [_coding_job(row) for row in rows]

    def update_status(self, job_name: str, status: str) -> None:
        cursor = self._connection.execute(
            "UPDATE coding_jobs SET status = ?, updated_at = ? WHERE job_name = ?",
            (status, _now(), job_name),
        )
        self._connection.commit()
        if cursor.rowcount != 1:
            raise RuntimeError(f"Coding Job not found: {job_name}")

    def set_metadata_value(self, job_name: str, key: str, value: str) -> None:
        self.set_metadata_values(job_name, {key: value})

    def remove_metadata_keys(self, job_name: str, keys: set[str]) -> None:
        try:
            self._connection.execute("BEGIN IMMEDIATE")
            row = self._connection.execute(
                "SELECT metadata FROM coding_jobs WHERE job_name = ?", (job_name,)
            ).fetchone()
            if row is None:
                raise RuntimeError(f"Coding Job not found: {job_name}")
            metadata = json.loads(row["metadata"] or "{}")
            for key in keys:
                metadata.pop(key, None)
            self._connection.execute(
                "UPDATE coding_jobs SET metadata = ?, updated_at = ? WHERE job_name = ?",
                (json.dumps(metadata, sort_keys=True), _now(), job_name),
            )
            self._connection.commit()
        except BaseException:
            self._connection.rollback()
            raise

    def set_metadata_values(self, job_name: str, values: dict[str, str]) -> None:
        try:
            self._connection.execute("BEGIN IMMEDIATE")
            row = self._connection.execute(
                "SELECT metadata FROM coding_jobs WHERE job_name = ?", (job_name,)
            ).fetchone()
            if row is None:
                raise RuntimeError(f"Coding Job not found: {job_name}")
            metadata = {**json.loads(row["metadata"] or "{}"), **values}
            self._connection.execute(
                "UPDATE coding_jobs SET metadata = ?, updated_at = ? WHERE job_name = ?",
                (json.dumps(metadata, sort_keys=True), _now(), job_name),
            )
            self._connection.commit()
        except BaseException:
            self._connection.rollback()
            raise

    def record_github_pr(self, job_name: str, number: int, url: str) -> None:
        cursor = self._connection.execute(
            """
            UPDATE coding_jobs
            SET github_pr_number = ?, github_pr_url = ?, updated_at = ?
            WHERE job_name = ?
            """,
            (number, url, _now(), job_name),
        )
        self._connection.commit()
        if cursor.rowcount != 1:
            raise RuntimeError(f"Coding Job not found: {job_name}")

    def record_acceptance_checklist(
        self,
        job_name: str,
        *,
        goal: str,
        items: tuple[AcceptanceItem, ...],
    ) -> AcceptanceChecklist:
        """Pin one admission checklist idempotently before implementation starts."""
        _validate_acceptance_items(items)
        now = _now()
        goal_digest = f"sha256:{hashlib.sha256(goal.encode()).hexdigest()}"
        encoded = json.dumps([asdict(item) for item in items], sort_keys=True)
        try:
            self._connection.execute(
                """
                INSERT INTO coding_acceptance_checklists (
                    job_name, goal_digest, items, state, revision, created_at, updated_at
                ) VALUES (?, ?, ?, 'draft', 1, ?, ?)
                """,
                (job_name, goal_digest, encoded, now, now),
            )
            self._connection.commit()
        except sqlite3.IntegrityError:
            self._connection.rollback()
            existing = self.get_acceptance_checklist(job_name)
            if (
                existing is None
                or existing.goal_digest != goal_digest
                or existing.items != items
            ):
                raise
            return existing
        recorded = self.get_acceptance_checklist(job_name)
        if recorded is None:
            raise RuntimeError("recorded acceptance checklist could not be loaded")
        return recorded

    def get_acceptance_checklist(self, job_name: str) -> AcceptanceChecklist | None:
        row = self._connection.execute(
            "SELECT * FROM coding_acceptance_checklists WHERE job_name = ?", (job_name,)
        ).fetchone()
        return _acceptance_checklist(row) if row is not None else None

    def replace_acceptance_checklist(
        self,
        job_name: str,
        items: tuple[AcceptanceItem, ...],
    ) -> AcceptanceChecklist:
        """Apply a human correction while the admitted checklist is still a draft."""
        _validate_acceptance_items(items)
        encoded = json.dumps([asdict(item) for item in items], sort_keys=True)
        cursor = self._connection.execute(
            """
            UPDATE coding_acceptance_checklists
            SET items = ?, revision = revision + 1, updated_at = ?
            WHERE job_name = ? AND state = 'draft'
            """,
            (encoded, _now(), job_name),
        )
        self._connection.commit()
        if cursor.rowcount != 1:
            existing = self.get_acceptance_checklist(job_name)
            if existing is None:
                raise RuntimeError(f"Acceptance checklist not found: {job_name}")
            raise RuntimeError(f"Acceptance checklist already governs completion: {job_name}")
        updated = self.get_acceptance_checklist(job_name)
        if updated is None:
            raise RuntimeError("updated acceptance checklist could not be loaded")
        return updated

    def freeze_acceptance_checklist(self, job_name: str) -> AcceptanceChecklist:
        """Make the corrected admission checklist govern verification completion."""
        cursor = self._connection.execute(
            """
            UPDATE coding_acceptance_checklists SET state = 'governing', updated_at = ?
            WHERE job_name = ? AND state = 'draft'
            """,
            (_now(), job_name),
        )
        self._connection.commit()
        checklist = self.get_acceptance_checklist(job_name)
        if checklist is None:
            raise RuntimeError(f"Acceptance checklist not found: {job_name}")
        if cursor.rowcount not in {0, 1} or checklist.state != "governing":
            raise RuntimeError(f"Could not freeze acceptance checklist: {job_name}")
        return checklist

    def create_command_run(
        self,
        job_name: str,
        kind: str,
        command: str,
        output_path: str,
    ) -> CodingCommandRun:
        now = _now()
        cursor = self._connection.execute(
            """
            INSERT INTO coding_command_runs (
                job_name, kind, command, status, exit_code, started_at,
                finished_at, output_path, git_commit_before, git_commit_after
            ) VALUES (?, ?, ?, 'running', NULL, ?, NULL, ?, NULL, NULL)
            """,
            (job_name, kind, command, now, output_path),
        )
        self._connection.commit()
        created = self.get_command_run(int(cursor.lastrowid))
        if created is None:
            raise RuntimeError("created coding command run could not be loaded")
        return created

    def finish_command_run(self, run_id: int, status: str, exit_code: int) -> CodingCommandRun:
        self._connection.execute(
            """
            UPDATE coding_command_runs
            SET status = ?, exit_code = ?, finished_at = ?
            WHERE id = ? AND status = 'running'
            """,
            (status, exit_code, _now(), run_id),
        )
        self._connection.commit()
        finished = self.get_command_run(run_id)
        if finished is None:
            raise RuntimeError("finished coding command run could not be loaded")
        return finished

    def set_command_run_output_path(self, run_id: int, output_path: str) -> CodingCommandRun:
        self._connection.execute(
            "UPDATE coding_command_runs SET output_path = ? WHERE id = ?",
            (output_path, run_id),
        )
        self._connection.commit()
        updated = self.get_command_run(run_id)
        if updated is None:
            raise RuntimeError("updated coding command run could not be loaded")
        return updated

    def set_command_run_git_commits(
        self,
        run_id: int,
        *,
        before: str | None,
        after: str | None,
    ) -> CodingCommandRun:
        self._connection.execute(
            """
            UPDATE coding_command_runs
            SET git_commit_before = ?, git_commit_after = ? WHERE id = ?
            """,
            (before, after, run_id),
        )
        self._connection.commit()
        updated = self.get_command_run(run_id)
        if updated is None:
            raise RuntimeError("updated coding command run could not be loaded")
        return updated

    def get_command_run(self, run_id: int) -> CodingCommandRun | None:
        row = self._connection.execute(
            "SELECT * FROM coding_command_runs WHERE id = ?", (run_id,)
        ).fetchone()
        return _coding_run(row) if row is not None else None

    def list_command_runs(self, job_name: str) -> list[CodingCommandRun]:
        rows = self._connection.execute(
            """
            SELECT * FROM coding_command_runs
            WHERE job_name = ? ORDER BY started_at DESC, id DESC
            """,
            (job_name,),
        ).fetchall()
        return [_coding_run(row) for row in rows]

    def record_command_run(
        self,
        job_name: str,
        command_name: str,
        command: str,
        status: str,
        exit_code: int,
        git_commit_before: str | None = None,
        git_commit_after: str | None = None,
    ) -> CodingCommandRun:
        run = self.create_command_run(job_name, command_name, command, "")
        run = self.finish_command_run(run.id, status, exit_code)
        return self.set_command_run_git_commits(
            run.id,
            before=git_commit_before,
            after=git_commit_after,
        )

    def _migrate(self) -> None:
        super()._migrate()
        self._connection.execute(
            """
            CREATE TABLE IF NOT EXISTS coding_jobs (
                job_name TEXT PRIMARY KEY,
                status TEXT NOT NULL,
                metadata TEXT NOT NULL,
                github_pr_number INTEGER,
                github_pr_url TEXT,
                created_at TEXT NOT NULL,
                updated_at TEXT NOT NULL
            )
            """
        )
        self._connection.execute(
            """
            CREATE TABLE IF NOT EXISTS coding_command_runs (
                id INTEGER PRIMARY KEY,
                job_name TEXT NOT NULL,
                kind TEXT NOT NULL,
                command TEXT NOT NULL,
                status TEXT NOT NULL,
                exit_code INTEGER,
                started_at TEXT NOT NULL,
                finished_at TEXT,
                output_path TEXT NOT NULL,
                git_commit_before TEXT,
                git_commit_after TEXT
            )
            """
        )
        self._connection.execute(
            """
            CREATE TABLE IF NOT EXISTS coding_acceptance_checklists (
                job_name TEXT PRIMARY KEY,
                goal_digest TEXT NOT NULL,
                items TEXT NOT NULL,
                state TEXT NOT NULL,
                revision INTEGER NOT NULL,
                created_at TEXT NOT NULL,
                updated_at TEXT NOT NULL
            )
            """
        )
        self._connection.commit()


def _coding_job(row) -> CodingJob:
    return CodingJob(
        job_name=row["job_name"],
        status=row["status"],
        metadata=json.loads(row["metadata"] or "{}"),
        github_pr_number=row["github_pr_number"],
        github_pr_url=row["github_pr_url"],
        created_at=row["created_at"],
        updated_at=row["updated_at"],
    )


def _coding_run(row) -> CodingCommandRun:
    return CodingCommandRun(
        id=row["id"],
        job_name=row["job_name"],
        kind=row["kind"],
        command=row["command"],
        status=row["status"],
        exit_code=row["exit_code"],
        started_at=row["started_at"],
        finished_at=row["finished_at"],
        output_path=row["output_path"],
        git_commit_before=row["git_commit_before"],
        git_commit_after=row["git_commit_after"],
    )


def _acceptance_checklist(row) -> AcceptanceChecklist:
    return AcceptanceChecklist(
        job_name=row["job_name"],
        goal_digest=row["goal_digest"],
        items=tuple(AcceptanceItem(**item) for item in json.loads(row["items"])),
        state=row["state"],
        revision=row["revision"],
        created_at=row["created_at"],
        updated_at=row["updated_at"],
    )


def _validate_acceptance_items(items: tuple[AcceptanceItem, ...]) -> None:
    keys = [item.key for item in items]
    if len(keys) != len(set(keys)):
        raise ValueError("Acceptance item keys must be unique")
    for item in items:
        if not item.key or not item.text.strip():
            raise ValueError("Acceptance items require a key and text")
        if item.source not in {"goal", "issue", "contract", "human"}:
            raise ValueError(f"Unsupported acceptance source: {item.source}")
        if item.verifier not in {"command", "manual"}:
            raise ValueError(f"Unsupported acceptance verifier: {item.verifier}")
        if item.verifier == "command" and not item.verifier_command:
            raise ValueError(
                f"{item.verifier.title()} acceptance items require an exact pinned command"
            )


def _now() -> str:
    return datetime.now(UTC).isoformat(timespec="microseconds")
