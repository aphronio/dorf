"""Coding-to-PR state keyed by independent Job identity."""

from __future__ import annotations

import hashlib
import json
from dataclasses import dataclass
from datetime import UTC, datetime, timedelta

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
class FollowupFeedback:
    id: int
    job_name: str
    kind: str
    ref_id: str
    comment_id: str | None
    status: str
    commit_sha: str | None
    created_at: str
    updated_at: str


@dataclass(frozen=True)
class AfkCoordinator:
    target_repo: str
    issue_number: int
    owner_token: str
    job_name: str | None
    status: str


@dataclass(frozen=True)
class PendingCodingAdmission:
    id: str
    status: str
    request: dict[str, object]
    attention: dict[str, str]
    proof_id: str | None
    job_name: str | None
    expires_at: str
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
        admission_attempt_id: str | None = None,
    ) -> CodingJob:
        now = _now()
        try:
            self._connection.execute("BEGIN IMMEDIATE")
            self._expire_coding_admissions(now)
            if admission_attempt_id is not None:
                attempt = self._connection.execute(
                    "SELECT status FROM coding_admissions WHERE id = ?",
                    (admission_attempt_id,),
                ).fetchone()
                if attempt is None or attempt["status"] != "approved":
                    self._connection.commit()
                    raise RuntimeError(
                        "coding admission has already been consumed"
                    )
            self._connection.execute(
                """
                INSERT INTO coding_jobs (
                    job_name, status, metadata,
                    github_pr_number, github_pr_url, created_at, updated_at
                ) VALUES (?, ?, ?, NULL, NULL, ?, ?)
                """,
                (job_name, status, json.dumps(metadata, sort_keys=True), now, now),
            )
            if admission_attempt_id is not None:
                proof_id = None
                raw_proof = metadata.get("admission_proof")
                if raw_proof is not None:
                    try:
                        candidate = json.loads(raw_proof).get("proof_id")
                    except (AttributeError, json.JSONDecodeError):
                        candidate = None
                    if isinstance(candidate, str):
                        proof_id = candidate
                cursor = self._connection.execute(
                    """
                    UPDATE coding_admissions
                    SET status = 'admitted', job_name = ?, proof_id = ?, updated_at = ?
                    WHERE id = ? AND status = 'approved' AND expires_at > ?
                    """,
                    (job_name, proof_id, now, admission_attempt_id, now),
                )
                if cursor.rowcount != 1:
                    raise RuntimeError(
                        "coding admission has already been consumed"
                    )
            self._connection.commit()
        except BaseException:
            self._connection.rollback()
            raise
        created = self.get_coding_job(job_name)
        if created is None:
            raise RuntimeError("created coding Job could not be loaded")
        return created

    def retain_pending_coding_admission(
        self,
        request: dict[str, object],
        attention: dict[str, str],
        *,
        ttl_seconds: int,
    ) -> tuple[PendingCodingAdmission, bool]:
        canonical_request = json.dumps(request, sort_keys=True, separators=(",", ":"))
        base_attempt_id = "admission-intent-" + hashlib.sha256(
            canonical_request.encode()
        ).hexdigest()[:24]
        now = datetime.now(UTC)
        timestamp = now.isoformat(timespec="microseconds")
        expires_at = (now + timedelta(seconds=ttl_seconds)).isoformat(
            timespec="microseconds"
        )
        try:
            self._connection.execute("BEGIN IMMEDIATE")
            self._expire_coding_admissions(timestamp)
            latest = self._connection.execute(
                """
                SELECT * FROM coding_admissions
                WHERE request = ?
                ORDER BY created_at DESC, id DESC
                LIMIT 1
                """,
                (canonical_request,),
            ).fetchone()
            if latest is not None and latest["status"] in {
                "pending",
                "approved",
                "admitted",
            }:
                self._connection.commit()
                return _pending_coding_admission(latest), False

            generation = 1
            attempt_id = base_attempt_id
            while self._connection.execute(
                "SELECT 1 FROM coding_admissions WHERE id = ?", (attempt_id,)
            ).fetchone() is not None:
                generation += 1
                attempt_id = f"{base_attempt_id}-{generation}"
            self._connection.execute(
                """
                INSERT INTO coding_admissions (
                    id, status, request, attention, proof_id, job_name,
                    expires_at, created_at, updated_at
                ) VALUES (?, 'pending', ?, ?, NULL, NULL, ?, ?, ?)
                """,
                (
                    attempt_id,
                    canonical_request,
                    json.dumps(attention, sort_keys=True),
                    expires_at,
                    timestamp,
                    timestamp,
                ),
            )
            self._connection.commit()
        except BaseException:
            self._connection.rollback()
            raise
        attempt = self.get_coding_admission(attempt_id)
        if attempt is None:
            raise RuntimeError("retained coding admission could not be loaded")
        return attempt, True

    def get_coding_admission(self, attempt_id: str) -> PendingCodingAdmission | None:
        row = self._connection.execute(
            "SELECT * FROM coding_admissions WHERE id = ?", (attempt_id,)
        ).fetchone()
        return _pending_coding_admission(row) if row is not None else None

    def get_coding_admission_for_request(
        self, request: dict[str, object]
    ) -> PendingCodingAdmission | None:
        """Expire stale intents, then match the latest with a discovery-time wildcard."""
        timestamp = _now()
        try:
            self._connection.execute("BEGIN IMMEDIATE")
            self._expire_coding_admissions(timestamp)
            rows = self._connection.execute(
                "SELECT * FROM coding_admissions ORDER BY created_at DESC, id DESC"
            ).fetchall()
            self._connection.commit()
        except BaseException:
            self._connection.rollback()
            raise
        for row in rows:
            retained = _pending_coding_admission(row)
            if all(
                key == "repository" and value is None
                or retained.request.get(key) == value
                for key, value in request.items()
            ):
                return retained
        return None

    def list_coding_admissions(self) -> list[PendingCodingAdmission]:
        rows = self._connection.execute(
            "SELECT * FROM coding_admissions ORDER BY created_at, id"
        ).fetchall()
        return [_pending_coding_admission(row) for row in rows]

    def approve_coding_admission(self, attempt_id: str) -> bool:
        timestamp = _now()
        try:
            self._connection.execute("BEGIN IMMEDIATE")
            self._expire_coding_admissions(timestamp)
            cursor = self._connection.execute(
                """
                UPDATE coding_admissions
                SET status = 'approved', updated_at = ?
                WHERE id = ? AND status = 'pending' AND expires_at > ?
                """,
                (timestamp, attempt_id, timestamp),
            )
            current = self._connection.execute(
                "SELECT status FROM coding_admissions WHERE id = ?", (attempt_id,)
            ).fetchone()
            self._connection.commit()
        except BaseException:
            self._connection.rollback()
            raise
        return cursor.rowcount == 1 or (
            current is not None and current["status"] in {"approved", "admitted"}
        )

    def end_pending_coding_admission(self, attempt_id: str, outcome: str) -> bool:
        if outcome not in {"declined", "expired"}:
            raise ValueError(f"Unsupported coding admission outcome: {outcome}")
        timestamp = _now()
        try:
            self._connection.execute("BEGIN IMMEDIATE")
            self._expire_coding_admissions(timestamp)
            cursor = self._connection.execute(
                """
                UPDATE coding_admissions SET status = ?, updated_at = ?
                WHERE id = ? AND status = 'pending'
                """,
                (outcome, timestamp, attempt_id),
            )
            self._connection.commit()
        except BaseException:
            self._connection.rollback()
            raise
        return cursor.rowcount == 1

    def _expire_coding_admissions(self, timestamp: str) -> None:
        self._connection.execute(
            """
            UPDATE coding_admissions SET status = 'expired', updated_at = ?
            WHERE status IN ('pending', 'approved') AND expires_at <= ?
            """,
            (timestamp, timestamp),
        )

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

    def interrupt_abandoned_afk_runs(self, job_name: str) -> list[CodingCommandRun]:
        rows = self._connection.execute(
            """
            UPDATE coding_command_runs
            SET status = 'interrupted', exit_code = 130, finished_at = ?
            WHERE job_name = ? AND status = 'running'
              AND (kind = 'afk' OR kind = 'check' OR kind LIKE 'review:%')
            RETURNING id
            """,
            (_now(), job_name),
        ).fetchall()
        self._connection.commit()
        return [run for row in rows if (run := self.get_command_run(int(row["id"]))) is not None]

    def claim_afk_coordinator(
        self,
        target_repo: str,
        issue_number: int,
        owner_token: str,
        *,
        takeover: bool = False,
        expected_job_name: str | None = None,
    ) -> AfkCoordinator:
        self._connection.execute("BEGIN IMMEDIATE")
        try:
            row = self._connection.execute(
                "SELECT * FROM afk_coordinators WHERE target_repo = ? AND issue_number = ?",
                (target_repo, issue_number),
            ).fetchone()
            if row is None:
                self._connection.execute(
                    """
                    INSERT INTO afk_coordinators (
                        target_repo, issue_number, owner_token, job_name, status
                    ) VALUES (?, ?, ?, NULL, 'running')
                    """,
                    (target_repo, issue_number, owner_token),
                )
            elif row["status"] == "running" and row["owner_token"] != owner_token and not takeover:
                raise RuntimeError("AFK coordinator is already running")
            elif expected_job_name is not None and row["job_name"] not in {None, expected_job_name}:
                raise RuntimeError(f"AFK reservation belongs to {row['job_name']}")
            else:
                clear_finished = row["status"] != "running" and expected_job_name is None
                self._connection.execute(
                    """
                    UPDATE afk_coordinators
                    SET owner_token = ?, status = 'running',
                        job_name = CASE WHEN ? THEN NULL ELSE job_name END
                    WHERE target_repo = ? AND issue_number = ?
                    """,
                    (owner_token, clear_finished, target_repo, issue_number),
                )
            self._connection.commit()
        except BaseException:
            self._connection.rollback()
            raise
        claimed = self.get_afk_coordinator(target_repo, issue_number)
        if claimed is None:
            raise RuntimeError("claimed AFK coordinator could not be loaded")
        return claimed

    def get_afk_coordinator(self, target_repo: str, issue_number: int) -> AfkCoordinator | None:
        row = self._connection.execute(
            "SELECT * FROM afk_coordinators WHERE target_repo = ? AND issue_number = ?",
            (target_repo, issue_number),
        ).fetchone()
        if row is None:
            return None
        return AfkCoordinator(
            target_repo=row["target_repo"],
            issue_number=row["issue_number"],
            owner_token=row["owner_token"],
            job_name=row["job_name"],
            status=row["status"],
        )

    def link_afk_job(
        self, target_repo: str, issue_number: int, owner_token: str, job_name: str
    ) -> None:
        cursor = self._connection.execute(
            """
            UPDATE afk_coordinators SET job_name = ?
            WHERE target_repo = ? AND issue_number = ? AND owner_token = ?
            """,
            (job_name, target_repo, issue_number, owner_token),
        )
        self._connection.commit()
        if cursor.rowcount != 1:
            raise RuntimeError("AFK coordinator ownership was lost")

    def release_unlinked_afk_coordinator(
        self, target_repo: str, issue_number: int, owner_token: str
    ) -> None:
        self._connection.execute(
            """
            DELETE FROM afk_coordinators
            WHERE target_repo = ? AND issue_number = ? AND owner_token = ?
              AND job_name IS NULL
            """,
            (target_repo, issue_number, owner_token),
        )
        self._connection.commit()

    def assert_afk_coordinator_owner(
        self, target_repo: str, issue_number: int, owner_token: str
    ) -> None:
        row = self._connection.execute(
            """
            SELECT 1 FROM afk_coordinators
            WHERE target_repo = ? AND issue_number = ? AND owner_token = ?
              AND status = 'running'
            """,
            (target_repo, issue_number, owner_token),
        ).fetchone()
        if row is None:
            raise RuntimeError("AFK coordinator ownership was lost")

    def finish_afk_coordinator(
        self, target_repo: str, issue_number: int, owner_token: str, status: str
    ) -> None:
        cursor = self._connection.execute(
            """
            UPDATE afk_coordinators SET status = ?
            WHERE target_repo = ? AND issue_number = ? AND owner_token = ?
            """,
            (status, target_repo, issue_number, owner_token),
        )
        self._connection.commit()
        if cursor.rowcount != 1:
            raise RuntimeError("AFK coordinator ownership was lost")

    def get_followup_feedback(
        self, job_name: str, kind: str, ref_id: str
    ) -> FollowupFeedback | None:
        row = self._connection.execute(
            """
            SELECT * FROM followup_feedback
            WHERE job_name = ? AND kind = ? AND ref_id = ?
            """,
            (job_name, kind, ref_id),
        ).fetchone()
        return _followup_feedback(row) if row is not None else None

    def list_followup_feedback(self, job_name: str) -> list[FollowupFeedback]:
        rows = self._connection.execute(
            """
            SELECT * FROM followup_feedback
            WHERE job_name = ? ORDER BY created_at DESC, id DESC
            """,
            (job_name,),
        ).fetchall()
        return [_followup_feedback(row) for row in rows]

    def record_followup_feedback(
        self,
        job_name: str,
        *,
        kind: str,
        ref_id: str,
        comment_id: str | None,
        status: str,
        commit_sha: str | None,
    ) -> FollowupFeedback:
        now = _now()
        self._connection.execute(
            """
            INSERT INTO followup_feedback (
                job_name, kind, ref_id, comment_id, status, commit_sha,
                created_at, updated_at
            ) VALUES (?, ?, ?, ?, ?, ?, ?, ?)
            ON CONFLICT(job_name, kind, ref_id) DO UPDATE SET
                comment_id = excluded.comment_id,
                status = excluded.status,
                commit_sha = excluded.commit_sha,
                updated_at = excluded.updated_at
            """,
            (job_name, kind, ref_id, comment_id, status, commit_sha, now, now),
        )
        self._connection.commit()
        recorded = self.get_followup_feedback(job_name, kind, ref_id)
        if recorded is None:
            raise RuntimeError("recorded followup feedback could not be loaded")
        return recorded

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
            CREATE TABLE IF NOT EXISTS followup_feedback (
                id INTEGER PRIMARY KEY,
                job_name TEXT NOT NULL,
                kind TEXT NOT NULL,
                ref_id TEXT NOT NULL,
                comment_id TEXT,
                status TEXT NOT NULL,
                commit_sha TEXT,
                created_at TEXT NOT NULL,
                updated_at TEXT NOT NULL,
                UNIQUE(job_name, kind, ref_id)
            )
            """
        )
        self._connection.execute(
            """
            CREATE TABLE IF NOT EXISTS afk_coordinators (
                target_repo TEXT NOT NULL,
                issue_number INTEGER NOT NULL,
                owner_token TEXT NOT NULL,
                job_name TEXT,
                status TEXT NOT NULL,
                PRIMARY KEY (target_repo, issue_number)
            )
            """
        )
        self._connection.execute(
            """
            CREATE TABLE IF NOT EXISTS coding_admissions (
                id TEXT PRIMARY KEY,
                status TEXT NOT NULL,
                request TEXT NOT NULL,
                attention TEXT NOT NULL,
                proof_id TEXT,
                job_name TEXT,
                expires_at TEXT NOT NULL,
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


def _followup_feedback(row) -> FollowupFeedback:
    return FollowupFeedback(
        id=row["id"],
        job_name=row["job_name"],
        kind=row["kind"],
        ref_id=row["ref_id"],
        comment_id=row["comment_id"],
        status=row["status"],
        commit_sha=row["commit_sha"],
        created_at=row["created_at"],
        updated_at=row["updated_at"],
    )


def _pending_coding_admission(row) -> PendingCodingAdmission:
    return PendingCodingAdmission(
        id=row["id"],
        status=row["status"],
        request=json.loads(row["request"]),
        attention=json.loads(row["attention"]),
        proof_id=row["proof_id"],
        job_name=row["job_name"],
        expires_at=row["expires_at"],
        created_at=row["created_at"],
        updated_at=row["updated_at"],
    )


def _now() -> str:
    return datetime.now(UTC).isoformat(timespec="microseconds")
