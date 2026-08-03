"""SQLite persistence for durable Workers, Rooms, Jobs, and Assignments."""

from __future__ import annotations

import fcntl
import hashlib
import json
import os
import secrets
import sqlite3
from collections.abc import Iterator
from contextlib import contextmanager
from datetime import UTC, datetime
from pathlib import Path

from .assigned_job import (
    Assignment,
    Job,
    JobBinding,
    JobConversation,
    JobInput,
    JobTurn,
    NewJob,
    validate_assignment_id,
)
from .documents import JobDocumentStore
from .job import JobDirectory, validate_job_name
from .worker import (
    Room,
    Worker,
    WorkerAlreadyAttachedError,
    WorkerBinding,
    WorkerConversation,
    WorkerMessage,
    WorkerOfflineError,
    WorkerPresence,
    WorkerTurn,
    validate_worker_name,
)


def default_database_path() -> Path:
    data_home = os.environ.get("XDG_DATA_HOME")
    if data_home:
        return Path(data_home) / "dorf" / "state.sqlite3"
    return Path.home() / ".local" / "share" / "dorf" / "state.sqlite3"


class RuntimeStore:
    def __init__(self, connection: sqlite3.Connection, database_path: Path) -> None:
        self._connection = connection
        self.database_path = database_path
        self.jobs = JobDirectory(database_path.parent / "jobs")
        self.documents = JobDocumentStore(self.jobs)
        self._connection.row_factory = sqlite3.Row
        self._migrate()

    @classmethod
    def open(cls, path: Path | None = None) -> RuntimeStore:
        database_path = path or default_database_path()
        database_path.parent.mkdir(parents=True, exist_ok=True)
        return cls(sqlite3.connect(database_path), database_path)

    def close(self) -> None:
        self._connection.close()

    def create_worker_with_room(
        self,
        *,
        name: str,
        harness_type: str,
        provenance: str,
        lifecycle_policy: str,
        room_id: str,
        room_type: str,
        provider_id: str,
        workspace: str,
        metadata: dict[str, str],
    ) -> tuple[WorkerBinding, bool]:
        """Atomically create one Worker identity and its initial Room."""
        validate_worker_name(name)
        if not all(
            (
                harness_type,
                provenance,
                lifecycle_policy,
                room_type,
                provider_id,
                workspace,
            )
        ):
            raise ValueError("Worker and Room bindings cannot be empty")
        created = False
        try:
            self._connection.execute("BEGIN IMMEDIATE")
            existing = self._connection.execute(
                "SELECT * FROM workers WHERE name = ?", (name,)
            ).fetchone()
            if existing is None:
                now = _now()
                self._connection.execute(
                    """
                    INSERT INTO rooms (
                        id, worker_name, room_type, provider_id, workspace,
                        status, error, metadata, created_at, updated_at
                    ) VALUES (?, ?, ?, ?, ?, 'provisioning', NULL, ?, ?, ?)
                    """,
                    (
                        room_id,
                        name,
                        room_type,
                        provider_id,
                        workspace,
                        json.dumps(metadata, sort_keys=True),
                        now,
                        now,
                    ),
                )
                self._connection.execute(
                    """
                    INSERT INTO workers (
                        name, harness_type, provenance, lifecycle_policy,
                        status, error, current_room_id, created_at, updated_at
                    ) VALUES (?, ?, ?, ?, 'pending', NULL, ?, ?, ?)
                    """,
                    (
                        name,
                        harness_type,
                        provenance,
                        lifecycle_policy,
                        room_id,
                        now,
                        now,
                    ),
                )
                created = True
            else:
                if existing["harness_type"] != harness_type:
                    raise ValueError("Worker name is already bound to a different harness type")
                if (
                    existing["provenance"] != provenance
                    or existing["lifecycle_policy"] != lifecycle_policy
                ):
                    raise ValueError(
                        "Worker name is already bound to different provenance or lifecycle policy"
                    )
                current_room_id = existing["current_room_id"]
                room = self._connection.execute(
                    "SELECT * FROM rooms WHERE id = ?", (current_room_id,)
                ).fetchone()
                if room is None:
                    raise RuntimeError("Worker current Room binding is missing")
                if (
                    room["room_type"] != room_type
                    or room["provider_id"] != provider_id
                    or room["workspace"] != workspace
                    or json.loads(room["metadata"] or "{}") != metadata
                ):
                    raise ValueError("Worker name is already bound to different Room configuration")
            self._connection.commit()
        except BaseException:
            self._connection.rollback()
            raise
        binding = self.get_worker_binding(name)
        if binding is None:
            raise RuntimeError("created Worker binding could not be loaded")
        return binding, created

    def get_worker(self, name: str) -> Worker | None:
        validate_worker_name(name)
        row = self._connection.execute("SELECT * FROM workers WHERE name = ?", (name,)).fetchone()
        if row is None:
            return None
        conversation = self._connection.execute(
            """
            SELECT native_conversation_id FROM worker_conversations
            WHERE worker_name = ? AND kind = 'general'
            """,
            (name,),
        ).fetchone()
        native_id = conversation["native_conversation_id"] if conversation else None
        return _worker_from_row(row, native_id)

    def get_room(self, room_id: str) -> Room | None:
        row = self._connection.execute("SELECT * FROM rooms WHERE id = ?", (room_id,)).fetchone()
        return _room_from_row(row) if row is not None else None

    def get_latest_room(self, worker_name: str) -> Room | None:
        validate_worker_name(worker_name)
        row = self._connection.execute(
            "SELECT * FROM rooms WHERE worker_name = ? ORDER BY created_at DESC, id DESC LIMIT 1",
            (worker_name,),
        ).fetchone()
        return _room_from_row(row) if row is not None else None

    def get_current_room(self, worker_name: str) -> Room | None:
        worker = self.get_worker(worker_name)
        if worker is None or worker.current_room_id is None:
            return None
        row = self._connection.execute(
            "SELECT * FROM rooms WHERE id = ?", (worker.current_room_id,)
        ).fetchone()
        return _room_from_row(row) if row is not None else None

    def mark_worker_room_absent(self, worker_name: str, room_id: str, error: str) -> Worker:
        """Record exact provider-body loss without manufacturing a replacement Room."""
        validate_worker_name(worker_name)
        try:
            self._connection.execute("BEGIN IMMEDIATE")
            worker = self._connection.execute(
                "SELECT current_room_id FROM workers WHERE name = ?", (worker_name,)
            ).fetchone()
            if worker is None:
                raise RuntimeError(f"Worker not found: {worker_name}")
            if worker["current_room_id"] not in {None, room_id}:
                raise RuntimeError("Worker current Room identity changed during recovery")
            room = self._connection.execute(
                "SELECT id FROM rooms WHERE id = ? AND worker_name = ?",
                (room_id, worker_name),
            ).fetchone()
            if room is None:
                raise RuntimeError("Recorded Worker Room is missing")
            now = _now()
            self._connection.execute(
                "UPDATE rooms SET status = 'absent', error = ?, updated_at = ? WHERE id = ?",
                (error, now, room_id),
            )
            self._connection.execute(
                """
                UPDATE workers SET current_room_id = NULL, status = 'offline',
                    error = ?, updated_at = ? WHERE name = ?
                """,
                (error, now, worker_name),
            )
            self._connection.execute(
                "DELETE FROM worker_presence WHERE worker_name = ?", (worker_name,)
            )
            self._connection.commit()
        except BaseException:
            self._connection.rollback()
            raise
        lost = self.get_worker(worker_name)
        if lost is None:
            raise RuntimeError(f"Worker not found after Room loss: {worker_name}")
        return lost

    def get_worker_binding(self, name: str) -> WorkerBinding | None:
        worker = self.get_worker(name)
        if worker is None:
            return None
        room = self.get_current_room(name)
        if room is None:
            return None
        return WorkerBinding(worker, room)

    def get_worker_presence(self, worker_name: str) -> WorkerPresence | None:
        validate_worker_name(worker_name)
        row = self._connection.execute(
            "SELECT * FROM worker_presence WHERE worker_name = ?", (worker_name,)
        ).fetchone()
        return _worker_presence_from_row(row) if row is not None else None

    def create_worker_presence(
        self,
        worker_name: str,
        *,
        room_id: str,
        attachment_id: str,
        workspace: str,
    ) -> WorkerPresence:
        """Atomically claim one human attachment to the Worker's current Room."""
        validate_worker_name(worker_name)
        if not attachment_id or not workspace:
            raise ValueError("Worker attachment identity and workspace cannot be empty")
        try:
            self._connection.execute("BEGIN IMMEDIATE")
            worker = self._connection.execute(
                "SELECT current_room_id, status FROM workers WHERE name = ?", (worker_name,)
            ).fetchone()
            if worker is None:
                raise RuntimeError(f"Worker not found: {worker_name}")
            if worker["current_room_id"] != room_id:
                raise RuntimeError("Worker current Room identity changed")
            room = self._connection.execute(
                "SELECT status FROM rooms WHERE id = ?", (room_id,)
            ).fetchone()
            if (
                worker["status"] not in {"ready", "assigned"}
                or room is None
                or room["status"] != "ready"
            ):
                raise WorkerOfflineError(f"Worker {worker_name} is offline")
            existing = self._connection.execute(
                "SELECT attachment_id FROM worker_presence WHERE worker_name = ?",
                (worker_name,),
            ).fetchone()
            if existing is not None:
                raise WorkerAlreadyAttachedError(f"Worker {worker_name} is already attached")
            now = _now()
            self._connection.execute(
                """
                INSERT INTO worker_presence (
                    attachment_id, worker_name, room_id, workspace, attached_at
                ) VALUES (?, ?, ?, ?, ?)
                """,
                (attachment_id, worker_name, room_id, workspace, now),
            )
            self._connection.commit()
        except BaseException:
            self._connection.rollback()
            raise
        presence = self.get_worker_presence(worker_name)
        if presence is None:
            raise RuntimeError("created Worker presence could not be loaded")
        return presence

    def clear_worker_presence(self, worker_name: str, attachment_id: str) -> bool:
        cursor = self._connection.execute(
            "DELETE FROM worker_presence WHERE worker_name = ? AND attachment_id = ?",
            (worker_name, attachment_id),
        )
        self._connection.commit()
        return cursor.rowcount == 1

    def begin_worker_end(self, worker_name: str, *, interrupt: bool) -> WorkerBinding:
        validate_worker_name(worker_name)
        try:
            self._connection.execute("BEGIN IMMEDIATE")
            worker = self._connection.execute(
                "SELECT * FROM workers WHERE name = ?", (worker_name,)
            ).fetchone()
            if worker is None:
                raise RuntimeError(f"Worker not found: {worker_name}")
            if worker["status"] == "ended":
                self._connection.commit()
                binding = self.get_worker_binding(worker_name)
                if binding is None:
                    raise RuntimeError("ended Worker has no active Room")
                return binding
            room_id = worker["current_room_id"]
            if room_id is None:
                lost_room = self._connection.execute(
                    """
                    SELECT id FROM rooms
                    WHERE worker_name = ? AND status IN ('absent', 'cleanup-failed')
                    ORDER BY created_at DESC, id DESC LIMIT 1
                    """,
                    (worker_name,),
                ).fetchone()
                if lost_room is None:
                    raise RuntimeError("Worker has no recorded lost Room to end")
                room_id = lost_room["id"]
            open_job = self._connection.execute(
                "SELECT job_name FROM assignments WHERE worker_name = ? AND status != 'ended'",
                (worker_name,),
            ).fetchone()
            if open_job is not None:
                raise RuntimeError(f"Worker has open Job {open_job['job_name']}")
            if interrupt:
                now = _now()
                self._connection.execute(
                    """
                    UPDATE worker_turns
                    SET status = 'interrupted', exit_code = 130, phase = 'finished',
                        error = 'Interrupted by Worker end', finished_at = ?
                    WHERE worker_name = ? AND status IN ('running', 'recovery-required')
                    """,
                    (now, worker_name),
                )
            self._connection.execute(
                "UPDATE workers SET status = 'ending', error = NULL, updated_at = ? WHERE name = ?",
                (_now(), worker_name),
            )
            self._connection.execute(
                "DELETE FROM worker_presence WHERE worker_name = ?", (worker_name,)
            )
            self._connection.commit()
        except BaseException:
            self._connection.rollback()
            raise
        worker = self.get_worker(worker_name)
        room = self.get_room(room_id)
        if worker is None or room is None:
            raise RuntimeError("ending Worker binding could not be loaded")
        return WorkerBinding(worker, room)

    def finish_worker_end(self, worker_name: str, room_id: str) -> Worker:
        try:
            self._connection.execute("BEGIN IMMEDIATE")
            worker = self._connection.execute(
                "SELECT current_room_id, status FROM workers WHERE name = ?", (worker_name,)
            ).fetchone()
            if worker is None:
                raise RuntimeError(f"Worker not found: {worker_name}")
            if worker["status"] == "ended":
                self._connection.commit()
                ended = self.get_worker(worker_name)
                assert ended is not None
                return ended
            if worker["current_room_id"] not in {None, room_id} or worker["status"] != "ending":
                raise RuntimeError("Worker ending identity changed")
            room = self._connection.execute(
                "SELECT worker_name FROM rooms WHERE id = ?", (room_id,)
            ).fetchone()
            if room is None or room["worker_name"] != worker_name:
                raise RuntimeError("Worker ending Room identity changed")
            now = _now()
            self._connection.execute(
                "UPDATE rooms SET status = 'destroyed', error = NULL, updated_at = ? WHERE id = ?",
                (now, room_id),
            )
            self._connection.execute(
                """
                UPDATE workers SET status = 'ended', current_room_id = NULL,
                    error = NULL, updated_at = ? WHERE name = ?
                """,
                (now, worker_name),
            )
            self._connection.execute(
                """
                UPDATE worker_conversations SET status = 'ended', error = NULL, updated_at = ?
                WHERE worker_name = ?
                """,
                (now, worker_name),
            )
            self._connection.commit()
        except BaseException:
            self._connection.rollback()
            raise
        ended = self.get_worker(worker_name)
        if ended is None:
            raise RuntimeError("ended Worker could not be loaded")
        return ended

    def clear_current_room(self, worker_name: str, room_id: str) -> Worker:
        """Fence a known unavailable Room while retaining Worker identity."""
        cursor = self._connection.execute(
            """
            UPDATE workers
            SET current_room_id = NULL, status = 'offline', error = NULL,
                updated_at = ?
            WHERE name = ? AND current_room_id = ?
            """,
            (_now(), worker_name, room_id),
        )
        self._connection.commit()
        worker = self.get_worker(worker_name)
        if worker is None:
            raise RuntimeError(f"Worker not found: {worker_name}")
        if cursor.rowcount != 1 and worker.current_room_id is not None:
            raise RuntimeError("Worker current Room identity changed")
        return worker

    def update_worker_status(self, name: str, status: str, error: str | None = None) -> Worker:
        self._connection.execute(
            """
            UPDATE workers
            SET status = ?, error = ?, updated_at = ?
            WHERE name = ?
            """,
            (status, error, _now(), name),
        )
        self._connection.commit()
        worker = self.get_worker(name)
        if worker is None:
            raise RuntimeError("updated Worker could not be loaded")
        return worker

    def update_room_status(self, room_id: str, status: str, error: str | None = None) -> Room:
        self._connection.execute(
            """
            UPDATE rooms
            SET status = ?, error = ?, updated_at = ?
            WHERE id = ?
            """,
            (status, error, _now(), room_id),
        )
        self._connection.commit()
        row = self._connection.execute("SELECT * FROM rooms WHERE id = ?", (room_id,)).fetchone()
        if row is None:
            raise RuntimeError("updated Room could not be loaded")
        return _room_from_row(row)

    def get_worker_conversation(self, worker_name: str) -> WorkerConversation | None:
        validate_worker_name(worker_name)
        row = self._connection.execute(
            """
            SELECT * FROM worker_conversations
            WHERE worker_name = ? AND kind = 'general'
            """,
            (worker_name,),
        ).fetchone()
        return _worker_conversation_from_row(row) if row is not None else None

    def enqueue_worker_message(
        self,
        worker_name: str,
        *,
        message_id: str,
        text: str,
        default_model: str,
        default_reasoning_effort: str,
        model: str | None = None,
        reasoning_effort: str | None = None,
    ) -> tuple[WorkerMessage, bool]:
        """Atomically create the lazy general conversation and append one input."""
        validate_worker_name(worker_name)
        if not message_id:
            raise ValueError("Worker message ID cannot be empty")
        if not text.strip():
            raise ValueError("Worker message cannot be empty")
        if not default_model or not default_reasoning_effort:
            raise ValueError("Worker conversation defaults cannot be empty")
        try:
            self._connection.execute("BEGIN IMMEDIATE")
            worker = self._connection.execute(
                "SELECT status FROM workers WHERE name = ?", (worker_name,)
            ).fetchone()
            if worker is None:
                raise RuntimeError(f"Worker not found: {worker_name}")
            if worker["status"] in {"ending", "ended"}:
                raise RuntimeError(f"Worker is not open: {worker_name}")
            conversation = self._connection.execute(
                """
                SELECT * FROM worker_conversations
                WHERE worker_name = ? AND kind = 'general'
                """,
                (worker_name,),
            ).fetchone()
            now = _now()
            if conversation is None:
                conversation_id = f"conversation-{secrets.token_hex(16)}"
                self._connection.execute(
                    """
                    INSERT INTO worker_conversations (
                        id, worker_name, kind, native_conversation_id,
                        model, reasoning_effort, status, error,
                        created_at, updated_at
                    ) VALUES (?, ?, 'general', NULL, ?, ?, 'idle', NULL, ?, ?)
                    """,
                    (
                        conversation_id,
                        worker_name,
                        default_model,
                        default_reasoning_effort,
                        now,
                        now,
                    ),
                )
                conversation_model = default_model
                conversation_reasoning = default_reasoning_effort
            else:
                conversation_id = conversation["id"]
                conversation_model = conversation["model"]
                conversation_reasoning = conversation["reasoning_effort"]
            resolved_model = model or conversation_model
            resolved_reasoning = reasoning_effort or conversation_reasoning
            existing = self._connection.execute(
                "SELECT * FROM worker_messages WHERE id = ?", (message_id,)
            ).fetchone()
            if existing is not None:
                message = _worker_message_from_row(existing)
                if (
                    message.worker_name != worker_name
                    or message.text != text
                    or message.model != resolved_model
                    or message.reasoning_effort != resolved_reasoning
                ):
                    raise ValueError("Worker message ID is already bound to different input")
                self._connection.commit()
                return message, False
            row = self._connection.execute(
                """
                SELECT COALESCE(MAX(sequence), 0) + 1 AS next_sequence
                FROM worker_messages WHERE conversation_id = ?
                """,
                (conversation_id,),
            ).fetchone()
            sequence = int(row["next_sequence"])
            self._connection.execute(
                """
                INSERT INTO worker_messages (
                    id, conversation_id, worker_name, sequence, text,
                    model, reasoning_effort, created_at
                ) VALUES (?, ?, ?, ?, ?, ?, ?, ?)
                """,
                (
                    message_id,
                    conversation_id,
                    worker_name,
                    sequence,
                    text,
                    resolved_model,
                    resolved_reasoning,
                    now,
                ),
            )
            self._connection.commit()
        except BaseException:
            self._connection.rollback()
            raise
        return WorkerMessage(
            id=message_id,
            conversation_id=conversation_id,
            worker_name=worker_name,
            sequence=sequence,
            text=text,
            model=resolved_model,
            reasoning_effort=resolved_reasoning,
            created_at=now,
        ), True

    def list_worker_messages(self, worker_name: str) -> list[WorkerMessage]:
        validate_worker_name(worker_name)
        rows = self._connection.execute(
            """
            SELECT * FROM worker_messages
            WHERE worker_name = ?
            ORDER BY sequence
            """,
            (worker_name,),
        ).fetchall()
        return [_worker_message_from_row(row) for row in rows]

    def get_worker_turn_by_message(self, worker_name: str, message_id: str) -> WorkerTurn | None:
        row = self._connection.execute(
            """
            SELECT * FROM worker_turns
            WHERE worker_name = ? AND message_id = ?
            """,
            (worker_name, message_id),
        ).fetchone()
        return _worker_turn_from_row(row) if row is not None else None

    def list_worker_turns(self, worker_name: str) -> list[WorkerTurn]:
        rows = self._connection.execute(
            """
            SELECT * FROM worker_turns
            WHERE worker_name = ? ORDER BY id
            """,
            (worker_name,),
        ).fetchall()
        return [_worker_turn_from_row(row) for row in rows]

    def reset_unsubmitted_worker_turn(self, turn_id: int) -> None:
        turn = self._require_worker_turn(turn_id)
        if turn.native_turn_id is not None or turn.phase not in {
            "admitted",
            "submitting",
            "recovery-required",
        }:
            raise RuntimeError("Worker turn cannot be safely reset for delivery")
        self._connection.execute("DELETE FROM worker_turns WHERE id = ?", (turn_id,))
        self._connection.commit()

    def admit_worker_turn(
        self, message: WorkerMessage, *, output_path: str
    ) -> tuple[WorkerTurn, bool]:
        digest = hashlib.sha256(message.text.encode()).hexdigest()
        try:
            self._connection.execute("BEGIN IMMEDIATE")
            existing = self._connection.execute(
                "SELECT * FROM worker_turns WHERE message_id = ?",
                (message.id,),
            ).fetchone()
            if existing is not None:
                turn = _worker_turn_from_row(existing)
                if (
                    turn.worker_name != message.worker_name
                    or turn.conversation_id != message.conversation_id
                    or turn.input_digest != digest
                    or turn.output_path != output_path
                ):
                    raise ValueError("Worker message is already bound to a different turn")
                self._connection.commit()
                return turn, False
            active = self._connection.execute(
                """
                SELECT * FROM worker_turns
                WHERE conversation_id = ?
                  AND status IN ('running', 'recovery-required')
                ORDER BY id LIMIT 1
                """,
                (message.conversation_id,),
            ).fetchone()
            if active is not None:
                self._connection.commit()
                return _worker_turn_from_row(active), False
            now = _now()
            cursor = self._connection.execute(
                """
                INSERT INTO worker_turns (
                    conversation_id, worker_name, message_id, status, exit_code,
                    output_path, input_digest, native_turn_baseline_id,
                    native_turn_id, phase, error, started_at, finished_at
                ) VALUES (?, ?, ?, 'running', NULL, ?, ?, NULL, NULL,
                          'admitted', NULL, ?, NULL)
                """,
                (
                    message.conversation_id,
                    message.worker_name,
                    message.id,
                    output_path,
                    digest,
                    now,
                ),
            )
            self._connection.commit()
        except BaseException:
            self._connection.rollback()
            raise
        created = self._connection.execute(
            "SELECT * FROM worker_turns WHERE id = ?", (cursor.lastrowid,)
        ).fetchone()
        if created is None:
            raise RuntimeError("admitted Worker turn could not be loaded")
        return _worker_turn_from_row(created), True

    def bind_worker_conversation(
        self, conversation_id: str, native_conversation_id: str
    ) -> WorkerConversation:
        if not native_conversation_id:
            raise ValueError("native Worker conversation identity cannot be empty")
        try:
            self._connection.execute("BEGIN IMMEDIATE")
            row = self._connection.execute(
                "SELECT * FROM worker_conversations WHERE id = ?",
                (conversation_id,),
            ).fetchone()
            if row is None:
                raise RuntimeError("Worker conversation could not be loaded")
            current = row["native_conversation_id"]
            if current not in {None, native_conversation_id}:
                raise RuntimeError(
                    "Worker conversation is already bound to another native identity"
                )
            self._connection.execute(
                """
                UPDATE worker_conversations
                SET native_conversation_id = ?, updated_at = ?
                WHERE id = ?
                """,
                (native_conversation_id, _now(), conversation_id),
            )
            self._connection.commit()
        except BaseException:
            self._connection.rollback()
            raise
        return self._require_worker_conversation_id(conversation_id)

    def prepare_worker_turn(
        self, turn_id: int, *, baseline_native_turn_id: str | None
    ) -> WorkerTurn:
        self._connection.execute(
            """
            UPDATE worker_turns
            SET phase = 'submitting', native_turn_baseline_id = ?
            WHERE id = ? AND status = 'running' AND phase = 'admitted'
            """,
            (baseline_native_turn_id, turn_id),
        )
        self._connection.commit()
        return self._require_worker_turn(turn_id)

    def start_worker_turn(self, turn_id: int, native_turn_id: str) -> WorkerTurn:
        if not native_turn_id:
            raise ValueError("native Worker turn identity cannot be empty")
        turn = self._require_worker_turn(turn_id)
        if turn.native_turn_id not in {None, native_turn_id}:
            raise RuntimeError("Worker turn is already bound to another native identity")
        self._connection.execute(
            """
            UPDATE worker_turns
            SET native_turn_id = ?, phase = 'running'
            WHERE id = ? AND status IN ('running', 'recovery-required')
              AND phase IN ('submitting', 'running', 'recovery-required')
            """,
            (native_turn_id, turn_id),
        )
        self._connection.commit()
        return self._require_worker_turn(turn_id)

    def finish_worker_turn(
        self,
        turn_id: int,
        *,
        status: str,
        exit_code: int,
        error: str | None,
    ) -> WorkerTurn:
        if status not in {
            "succeeded",
            "failed",
            "interrupted",
            "recovery-required",
        }:
            raise ValueError(f"unsupported Worker turn status: {status}")
        finished_at = None if status == "recovery-required" else _now()
        phase = "recovery-required" if status == "recovery-required" else "finished"
        self._connection.execute(
            """
            UPDATE worker_turns
            SET status = ?, exit_code = ?, error = ?, phase = ?, finished_at = ?
            WHERE id = ? AND status IN ('running', 'recovery-required')
            """,
            (status, exit_code, error, phase, finished_at, turn_id),
        )
        self._connection.commit()
        return self._require_worker_turn(turn_id)

    def update_worker_conversation_status(
        self, conversation_id: str, status: str, error: str | None = None
    ) -> WorkerConversation:
        self._connection.execute(
            """
            UPDATE worker_conversations
            SET status = ?, error = ?, updated_at = ?
            WHERE id = ? AND (status != 'ended' OR ? = 'ended')
            """,
            (status, error, _now(), conversation_id, status),
        )
        self._connection.commit()
        return self._require_worker_conversation_id(conversation_id)

    def _require_worker_conversation_id(self, conversation_id: str) -> WorkerConversation:
        row = self._connection.execute(
            "SELECT * FROM worker_conversations WHERE id = ?",
            (conversation_id,),
        ).fetchone()
        if row is None:
            raise RuntimeError("Worker conversation could not be loaded")
        return _worker_conversation_from_row(row)

    def _require_worker_turn(self, turn_id: int) -> WorkerTurn:
        row = self._connection.execute(
            "SELECT * FROM worker_turns WHERE id = ?", (turn_id,)
        ).fetchone()
        if row is None:
            raise RuntimeError("Worker turn could not be loaded")
        return _worker_turn_from_row(row)

    def assign_job(self, new_job: NewJob) -> tuple[JobBinding, bool]:
        """Atomically create a goal-backed Job, Assignment, and initial input."""
        validate_job_name(new_job.name)
        validate_worker_name(new_job.worker_name)
        if not new_job.goal.strip():
            raise ValueError("Job goal cannot be empty")
        if not new_job.model or not new_job.reasoning_effort:
            raise ValueError("Job conversation defaults cannot be empty")
        created = False
        try:
            self._connection.execute("BEGIN IMMEDIATE")
            existing = self._connection.execute(
                "SELECT * FROM jobs WHERE name = ?", (new_job.name,)
            ).fetchone()
            if existing is not None:
                binding = self._job_binding_from_name(new_job.name)
                if (
                    binding.assignment.worker_name != new_job.worker_name
                    or binding.job.goal != new_job.goal
                    or binding.conversation.model != new_job.model
                    or binding.conversation.reasoning_effort != new_job.reasoning_effort
                ):
                    raise RuntimeError(f"Job {new_job.name} already has a different assignment")
                self._connection.commit()
                return binding, False

            worker = self._connection.execute(
                "SELECT * FROM workers WHERE name = ?", (new_job.worker_name,)
            ).fetchone()
            if worker is None:
                raise RuntimeError(f"Worker not found: {new_job.worker_name}")
            if worker["status"] != "ready" or worker["current_room_id"] is None:
                raise RuntimeError(f"Worker is not ready: {new_job.worker_name}")
            room = self._connection.execute(
                "SELECT * FROM rooms WHERE id = ?", (worker["current_room_id"],)
            ).fetchone()
            if room is None or room["status"] != "ready":
                raise RuntimeError(f"Worker Room is not ready: {new_job.worker_name}")
            active = self._connection.execute(
                """
                SELECT job_name FROM assignments
                WHERE worker_name = ? AND status != 'ended'
                """,
                (new_job.worker_name,),
            ).fetchone()
            if active is not None:
                raise RuntimeError(
                    f"Worker {new_job.worker_name} already has open Job {active['job_name']}"
                )

            now = _now()
            published = self.jobs.get(new_job.name)
            if published is None:
                assignment_id = f"assignment-{secrets.token_hex(16)}"
            else:
                assignment_id = published.assignment.get("id")
                if not isinstance(assignment_id, str) or not assignment_id:
                    raise RuntimeError(f"Job {new_job.name} document has no Assignment identity")
            validate_assignment_id(assignment_id)
            conversation_id = f"conversation-{secrets.token_hex(16)}"
            input_id = f"jmsg-{secrets.token_hex(16)}"
            workspace = f"{room['workspace'].rstrip('/')}/jobs/{new_job.name}"
            cursor = self._connection.execute(
                """
                INSERT INTO jobs (
                    name, status, goal_version, goal, created_at, updated_at
                ) VALUES (?, 'open', 1, ?, ?, ?)
                """,
                (new_job.name, new_job.goal, now, now),
            )
            self._connection.execute(
                """
                INSERT INTO assignments (
                    id, job_name, worker_name, generation, status, room_id,
                    workspace, started_at, ended_at
                ) VALUES (?, ?, ?, 1, 'preparing', ?, ?, ?, NULL)
                """,
                (
                    assignment_id,
                    new_job.name,
                    new_job.worker_name,
                    room["id"],
                    workspace,
                    now,
                ),
            )
            self._connection.execute(
                """
                INSERT INTO job_conversations (
                    id, job_name, native_conversation_id, model,
                    reasoning_effort, status, error, created_at, updated_at
                ) VALUES (?, ?, NULL, ?, ?, 'idle', NULL, ?, ?)
                """,
                (
                    conversation_id,
                    new_job.name,
                    new_job.model,
                    new_job.reasoning_effort,
                    now,
                    now,
                ),
            )
            self._connection.execute(
                """
                INSERT INTO job_inputs (
                    id, conversation_id, job_name, sequence, kind, goal_version,
                    text, model, reasoning_effort, created_at
                ) VALUES (?, ?, ?, 1, 'goal', 1, ?, ?, ?, ?)
                """,
                (
                    input_id,
                    conversation_id,
                    new_job.name,
                    new_job.goal,
                    new_job.model,
                    new_job.reasoning_effort,
                    now,
                ),
            )
            self._connection.execute(
                """
                UPDATE workers SET status = 'assigned', error = NULL, updated_at = ?
                WHERE name = ? AND status = 'ready'
                """,
                (now, new_job.worker_name),
            )
            self.jobs.create_assigned(
                name=new_job.name,
                goal=new_job.goal,
                worker_name=new_job.worker_name,
                room_id=room["id"],
                workspace=workspace,
                assignment_id=assignment_id,
                assignment_generation=1,
            )
            self._connection.commit()
            created = cursor.lastrowid is not None
        except BaseException:
            self._connection.rollback()
            raise
        return self._job_binding_from_name(new_job.name), created

    def get_job(self, name: str) -> Job | None:
        validate_job_name(name)
        row = self._connection.execute("SELECT * FROM jobs WHERE name = ?", (name,)).fetchone()
        return _job_from_row(row) if row is not None else None

    def get_assignment(self, job_name: str) -> Assignment | None:
        validate_job_name(job_name)
        row = self._connection.execute(
            "SELECT * FROM assignments WHERE job_name = ? ORDER BY generation DESC LIMIT 1",
            (job_name,),
        ).fetchone()
        return _assignment_from_row(row) if row is not None else None

    def get_job_conversation(self, job_name: str) -> JobConversation | None:
        validate_job_name(job_name)
        row = self._connection.execute(
            "SELECT * FROM job_conversations WHERE job_name = ?", (job_name,)
        ).fetchone()
        return _job_conversation_from_row(row) if row is not None else None

    def list_job_inputs(self, job_name: str) -> list[JobInput]:
        validate_job_name(job_name)
        rows = self._connection.execute(
            "SELECT * FROM job_inputs WHERE job_name = ? ORDER BY sequence",
            (job_name,),
        ).fetchall()
        return [_job_input_from_row(row) for row in rows]

    def get_open_job_for_worker(self, worker_name: str) -> str | None:
        validate_worker_name(worker_name)
        row = self._connection.execute(
            """
            SELECT job_name FROM assignments
            WHERE worker_name = ? AND status != 'ended'
            """,
            (worker_name,),
        ).fetchone()
        return row["job_name"] if row is not None else None

    def get_job_binding(self, name: str) -> JobBinding | None:
        if self.get_job(name) is None:
            return None
        return self._job_binding_from_name(name)

    def _job_binding_from_name(self, name: str) -> JobBinding:
        job = self.get_job(name)
        assignment = self.get_assignment(name)
        conversation = self.get_job_conversation(name)
        if job is None or assignment is None or conversation is None:
            raise RuntimeError(f"Job binding is incomplete: {name}")
        worker = self.get_worker(assignment.worker_name)
        room_row = self._connection.execute(
            "SELECT * FROM rooms WHERE id = ?", (assignment.room_id,)
        ).fetchone()
        if worker is None or room_row is None:
            raise RuntimeError(f"Job Worker/Room binding is missing: {name}")
        return JobBinding(job, assignment, conversation, worker, _room_from_row(room_row))

    def update_assignment_status(self, job_name: str, status: str) -> Assignment:
        if status not in {"preparing", "open", "workspace-failed"}:
            raise ValueError(f"unsupported Assignment status: {status}")
        self._connection.execute(
            """
            UPDATE assignments SET status = ?
            WHERE job_name = ? AND generation = (
                SELECT MAX(generation) FROM assignments WHERE job_name = ?
            )
            """,
            (status, job_name, job_name),
        )
        self._connection.commit()
        assignment = self.get_assignment(job_name)
        if assignment is None:
            raise RuntimeError("updated Assignment could not be loaded")
        return assignment

    def record_job_event(
        self,
        job_name: str,
        *,
        event_id: str,
        source: str,
        provenance: str,
        kind: str,
        summary: str,
        related: dict[str, str],
    ) -> None:
        self.documents.append_event(
            job_name,
            event_id=event_id,
            source=source,
            provenance=provenance,
            kind=kind,
            summary=summary,
            related=related,
        )

    def enqueue_job_input(
        self,
        job_name: str,
        *,
        message_id: str,
        text: str,
        model: str | None = None,
        reasoning_effort: str | None = None,
    ) -> tuple[JobInput, bool]:
        validate_job_name(job_name)
        if not message_id:
            raise ValueError("Job message ID cannot be empty")
        if not text.strip():
            raise ValueError("Job message cannot be empty")
        try:
            self._connection.execute("BEGIN IMMEDIATE")
            job = self._connection.execute(
                "SELECT status FROM jobs WHERE name = ?", (job_name,)
            ).fetchone()
            if job is None:
                raise RuntimeError(f"Job not found: {job_name}")
            assignment = self._connection.execute(
                """
                SELECT status FROM assignments
                WHERE job_name = ? ORDER BY generation DESC LIMIT 1
                """,
                (job_name,),
            ).fetchone()
            if job["status"] != "open" or assignment is None or assignment["status"] != "open":
                raise RuntimeError(f"Job is not open: {job_name}")
            conversation = self._connection.execute(
                "SELECT * FROM job_conversations WHERE job_name = ?", (job_name,)
            ).fetchone()
            if conversation is None:
                raise RuntimeError(f"Job conversation is missing: {job_name}")
            resolved_model = model or conversation["model"]
            resolved_reasoning = reasoning_effort or conversation["reasoning_effort"]
            existing = self._connection.execute(
                "SELECT * FROM job_inputs WHERE id = ?", (message_id,)
            ).fetchone()
            if existing is not None:
                job_input = _job_input_from_row(existing)
                if (
                    job_input.job_name != job_name
                    or job_input.kind != "message"
                    or job_input.text != text
                    or job_input.model != resolved_model
                    or job_input.reasoning_effort != resolved_reasoning
                ):
                    raise ValueError("Job message ID is already bound to different input")
                self._connection.commit()
                return job_input, False
            row = self._connection.execute(
                """
                SELECT COALESCE(MAX(sequence), 0) + 1 AS next_sequence
                FROM job_inputs WHERE conversation_id = ?
                """,
                (conversation["id"],),
            ).fetchone()
            sequence = int(row["next_sequence"])
            now = _now()
            self._connection.execute(
                """
                INSERT INTO job_inputs (
                    id, conversation_id, job_name, sequence, kind, goal_version,
                    text, model, reasoning_effort, created_at
                ) VALUES (?, ?, ?, ?, 'message', NULL, ?, ?, ?, ?)
                """,
                (
                    message_id,
                    conversation["id"],
                    job_name,
                    sequence,
                    text,
                    resolved_model,
                    resolved_reasoning,
                    now,
                ),
            )
            self._connection.commit()
        except BaseException:
            self._connection.rollback()
            raise
        return JobInput(
            id=message_id,
            conversation_id=conversation["id"],
            job_name=job_name,
            sequence=sequence,
            kind="message",
            goal_version=None,
            text=text,
            model=resolved_model,
            reasoning_effort=resolved_reasoning,
            created_at=now,
        ), True

    def get_job_turn_by_input(self, job_name: str, input_id: str) -> JobTurn | None:
        row = self._connection.execute(
            "SELECT * FROM job_turns WHERE job_name = ? AND input_id = ?",
            (job_name, input_id),
        ).fetchone()
        return _job_turn_from_row(row) if row is not None else None

    def list_job_turns(self, job_name: str) -> list[JobTurn]:
        rows = self._connection.execute(
            "SELECT * FROM job_turns WHERE job_name = ? ORDER BY id",
            (job_name,),
        ).fetchall()
        return [_job_turn_from_row(row) for row in rows]

    def begin_job_end(self, job_name: str, *, interrupt: bool) -> JobInput:
        """Durably close admission and append one stable cooperative cleanup input."""
        validate_job_name(job_name)
        cleanup_id = f"jmsg-end-{hashlib.sha256(job_name.encode()).hexdigest()[:32]}"
        prompt = (
            "Dorf is ending this Job. Stop services and background work started for this Job, "
            "leave the workspace safe to remove, report anything that could not be cleaned, and "
            "then confirm cleanup is complete."
        )
        try:
            self._connection.execute("BEGIN IMMEDIATE")
            job = self._connection.execute(
                "SELECT status FROM jobs WHERE name = ?", (job_name,)
            ).fetchone()
            if job is None:
                raise RuntimeError(f"Job not found: {job_name}")
            if job["status"] == "ended":
                existing = self._connection.execute(
                    "SELECT * FROM job_inputs WHERE id = ?", (cleanup_id,)
                ).fetchone()
                if existing is None:
                    raise RuntimeError("ended Job is missing its cleanup input")
                self._connection.commit()
                return _job_input_from_row(existing)
            assignment = self._connection.execute(
                "SELECT * FROM assignments WHERE job_name = ? ORDER BY generation DESC LIMIT 1",
                (job_name,),
            ).fetchone()
            conversation = self._connection.execute(
                "SELECT * FROM job_conversations WHERE job_name = ?", (job_name,)
            ).fetchone()
            if assignment is None or conversation is None:
                raise RuntimeError("Job ending binding is incomplete")
            if not interrupt and job["status"] == "open":
                unsettled = self._connection.execute(
                    """
                    SELECT 1 FROM job_inputs i
                    LEFT JOIN job_turns t ON t.input_id = i.id
                    WHERE i.job_name = ? AND i.kind != 'cleanup'
                      AND (t.id IS NULL OR t.status IN ('running', 'recovery-required'))
                    LIMIT 1
                    """,
                    (job_name,),
                ).fetchone()
                if unsettled is not None:
                    raise RuntimeError(f"Job has unsettled input: {job_name}")
            now = _now()
            if interrupt:
                self._connection.execute(
                    """
                    UPDATE job_turns
                    SET status = 'interrupted', exit_code = 130, phase = 'finished',
                        error = 'Interrupted by Job end', finished_at = ?
                    WHERE job_name = ? AND status IN ('running', 'recovery-required')
                    """,
                    (now, job_name),
                )
            existing = self._connection.execute(
                "SELECT * FROM job_inputs WHERE id = ?", (cleanup_id,)
            ).fetchone()
            if existing is None:
                sequence = int(
                    self._connection.execute(
                        "SELECT COALESCE(MAX(sequence), 0) + 1 FROM job_inputs WHERE job_name = ?",
                        (job_name,),
                    ).fetchone()[0]
                )
                self._connection.execute(
                    """
                    INSERT INTO job_inputs (
                        id, conversation_id, job_name, sequence, kind, goal_version,
                        text, model, reasoning_effort, created_at
                    ) VALUES (?, ?, ?, ?, 'cleanup', NULL, ?, ?, ?, ?)
                    """,
                    (
                        cleanup_id,
                        conversation["id"],
                        job_name,
                        sequence,
                        prompt,
                        conversation["model"],
                        conversation["reasoning_effort"],
                        now,
                    ),
                )
            self._connection.execute(
                "UPDATE jobs SET status = 'ending', updated_at = ? WHERE name = ?",
                (now, job_name),
            )
            self._connection.execute(
                "UPDATE assignments SET status = 'ending' WHERE id = ?",
                (assignment["id"],),
            )
            self._connection.commit()
        except BaseException:
            self._connection.rollback()
            raise
        row = self._connection.execute(
            "SELECT * FROM job_inputs WHERE id = ?", (cleanup_id,)
        ).fetchone()
        return _job_input_from_row(row)

    def retry_job_cleanup_turn(self, input_id: str) -> None:
        turn = self._connection.execute(
            "SELECT status FROM job_turns WHERE input_id = ?", (input_id,)
        ).fetchone()
        if turn is None:
            return
        if turn["status"] not in {"failed", "interrupted"}:
            raise RuntimeError("Job cleanup turn is not retryable")
        self._connection.execute("DELETE FROM job_turns WHERE input_id = ?", (input_id,))
        self._connection.commit()

    def finish_job_end(
        self, job_name: str, cleanup_input_id: str, *, interrupted: bool
    ) -> JobBinding:
        try:
            self._connection.execute("BEGIN IMMEDIATE")
            turn = self._connection.execute(
                "SELECT status FROM job_turns WHERE input_id = ?",
                (cleanup_input_id,),
            ).fetchone()
            if not interrupted and (turn is None or turn["status"] != "succeeded"):
                raise RuntimeError("Job cleanup turn has not succeeded")
            assignment = self._connection.execute(
                "SELECT * FROM assignments WHERE job_name = ? ORDER BY generation DESC LIMIT 1",
                (job_name,),
            ).fetchone()
            if assignment is None:
                raise RuntimeError("Job Assignment is missing")
            now = _now()
            self._connection.execute(
                "UPDATE assignments SET status = 'ended', ended_at = ? WHERE id = ?",
                (now, assignment["id"]),
            )
            self._connection.execute(
                "UPDATE jobs SET status = 'ended', updated_at = ? WHERE name = ?",
                (now, job_name),
            )
            self._connection.execute(
                """
                UPDATE job_conversations SET status = 'ended', error = NULL, updated_at = ?
                WHERE job_name = ?
                """,
                (now, job_name),
            )
            self._connection.execute(
                """
                UPDATE workers SET status = 'ready', error = NULL, updated_at = ?
                WHERE name = ? AND current_room_id = ?
                """,
                (now, assignment["worker_name"], assignment["room_id"]),
            )
            self._connection.commit()
        except BaseException:
            self._connection.rollback()
            raise
        return self._job_binding_from_name(job_name)

    def reset_unsubmitted_job_turn(self, turn_id: int) -> None:
        turn = self._require_job_turn(turn_id)
        if turn.native_turn_id is not None or turn.phase not in {
            "admitted",
            "submitting",
            "recovery-required",
        }:
            raise RuntimeError("Job turn cannot be safely reset for delivery")
        self._connection.execute("DELETE FROM job_turns WHERE id = ?", (turn_id,))
        self._connection.commit()

    def admit_job_turn(self, job_input: JobInput, *, output_path: str) -> tuple[JobTurn, bool]:
        digest = hashlib.sha256(job_input.text.encode()).hexdigest()
        try:
            self._connection.execute("BEGIN IMMEDIATE")
            existing = self._connection.execute(
                "SELECT * FROM job_turns WHERE input_id = ?", (job_input.id,)
            ).fetchone()
            if existing is not None:
                turn = _job_turn_from_row(existing)
                if (
                    turn.job_name != job_input.job_name
                    or turn.conversation_id != job_input.conversation_id
                    or turn.input_digest != digest
                    or turn.output_path != output_path
                ):
                    raise ValueError("Job input is already bound to a different turn")
                self._connection.commit()
                return turn, False
            active = self._connection.execute(
                """
                SELECT * FROM job_turns
                WHERE conversation_id = ?
                  AND status IN ('running', 'recovery-required')
                ORDER BY id LIMIT 1
                """,
                (job_input.conversation_id,),
            ).fetchone()
            if active is not None:
                self._connection.commit()
                return _job_turn_from_row(active), False
            now = _now()
            cursor = self._connection.execute(
                """
                INSERT INTO job_turns (
                    conversation_id, job_name, input_id, status, exit_code,
                    output_path, input_digest, native_turn_baseline_id,
                    native_turn_id, phase, error, started_at, finished_at
                ) VALUES (?, ?, ?, 'running', NULL, ?, ?, NULL, NULL,
                          'admitted', NULL, ?, NULL)
                """,
                (
                    job_input.conversation_id,
                    job_input.job_name,
                    job_input.id,
                    output_path,
                    digest,
                    now,
                ),
            )
            self._connection.commit()
        except BaseException:
            self._connection.rollback()
            raise
        row = self._connection.execute(
            "SELECT * FROM job_turns WHERE id = ?", (cursor.lastrowid,)
        ).fetchone()
        if row is None:
            raise RuntimeError("admitted Job turn could not be loaded")
        return _job_turn_from_row(row), True

    def bind_job_conversation(
        self, conversation_id: str, native_conversation_id: str
    ) -> JobConversation:
        if not native_conversation_id:
            raise ValueError("native Job conversation identity cannot be empty")
        current = self._require_job_conversation_id(conversation_id)
        if current.native_conversation_id not in {None, native_conversation_id}:
            raise RuntimeError("Job conversation is already bound to another native identity")
        self._connection.execute(
            """
            UPDATE job_conversations
            SET native_conversation_id = ?, updated_at = ? WHERE id = ?
            """,
            (native_conversation_id, _now(), conversation_id),
        )
        self._connection.commit()
        return self._require_job_conversation_id(conversation_id)

    def prepare_job_turn(self, turn_id: int, *, baseline_native_turn_id: str | None) -> JobTurn:
        self._connection.execute(
            """
            UPDATE job_turns SET phase = 'submitting', native_turn_baseline_id = ?
            WHERE id = ? AND status = 'running' AND phase = 'admitted'
            """,
            (baseline_native_turn_id, turn_id),
        )
        self._connection.commit()
        return self._require_job_turn(turn_id)

    def start_job_turn(self, turn_id: int, native_turn_id: str) -> JobTurn:
        if not native_turn_id:
            raise ValueError("native Job turn identity cannot be empty")
        turn = self._require_job_turn(turn_id)
        if turn.native_turn_id not in {None, native_turn_id}:
            raise RuntimeError("Job turn is already bound to another native identity")
        self._connection.execute(
            """
            UPDATE job_turns SET native_turn_id = ?, phase = 'running'
            WHERE id = ? AND status IN ('running', 'recovery-required')
              AND phase IN ('submitting', 'running', 'recovery-required')
            """,
            (native_turn_id, turn_id),
        )
        self._connection.commit()
        return self._require_job_turn(turn_id)

    def finish_job_turn(
        self,
        turn_id: int,
        *,
        status: str,
        exit_code: int,
        error: str | None,
    ) -> JobTurn:
        if status not in {
            "succeeded",
            "failed",
            "interrupted",
            "recovery-required",
        }:
            raise ValueError(f"unsupported Job turn status: {status}")
        finished_at = None if status == "recovery-required" else _now()
        phase = "recovery-required" if status == "recovery-required" else "finished"
        self._connection.execute(
            """
            UPDATE job_turns
            SET status = ?, exit_code = ?, error = ?, phase = ?, finished_at = ?
            WHERE id = ? AND status IN ('running', 'recovery-required')
            """,
            (status, exit_code, error, phase, finished_at, turn_id),
        )
        self._connection.commit()
        return self._require_job_turn(turn_id)

    def update_job_conversation_status(
        self, conversation_id: str, status: str, error: str | None = None
    ) -> JobConversation:
        self._connection.execute(
            """
            UPDATE job_conversations SET status = ?, error = ?, updated_at = ?
            WHERE id = ? AND (status != 'ended' OR ? = 'ended')
            """,
            (status, error, _now(), conversation_id, status),
        )
        self._connection.commit()
        return self._require_job_conversation_id(conversation_id)

    def _require_job_conversation_id(self, conversation_id: str) -> JobConversation:
        row = self._connection.execute(
            "SELECT * FROM job_conversations WHERE id = ?", (conversation_id,)
        ).fetchone()
        if row is None:
            raise RuntimeError("Job conversation could not be loaded")
        return _job_conversation_from_row(row)

    def _require_job_turn(self, turn_id: int) -> JobTurn:
        row = self._connection.execute(
            "SELECT * FROM job_turns WHERE id = ?", (turn_id,)
        ).fetchone()
        if row is None:
            raise RuntimeError("Job turn could not be loaded")
        return _job_turn_from_row(row)

    @contextmanager
    def worker_spawn_lock(self, worker_name: str) -> Iterator[None]:
        """Serialize initial provisioning and preparation for one Worker name."""
        validate_worker_name(worker_name)
        with self._named_process_lock(worker_name, "worker-spawn"):
            yield

    @contextmanager
    def worker_attachment_lock(self, worker_name: str) -> Iterator[bool]:
        """Claim the one live local human attachment process for a Worker."""
        validate_worker_name(worker_name)
        with self._named_process_lock(
            worker_name,
            "human-attachment",
            blocking=False,
        ) as acquired:
            yield acquired

    @contextmanager
    def worker_message_dispatcher_lock(self, worker_name: str) -> Iterator[None]:
        """Serialize direct-message delivery for one Worker conversation."""
        validate_worker_name(worker_name)
        with self._named_process_lock(worker_name, "worker-messages-dispatch"):
            yield

    @contextmanager
    def job_assignment_lock(self, job_name: str) -> Iterator[None]:
        """Serialize one Job identity's assignment and workspace preparation."""
        validate_job_name(job_name)
        with self._named_process_lock(job_name, "job-assignment"):
            yield

    @contextmanager
    def job_input_dispatcher_lock(self, job_name: str) -> Iterator[None]:
        """Serialize input delivery for one independent Job conversation."""
        validate_job_name(job_name)
        with self._named_process_lock(job_name, "job-inputs-dispatch"):
            yield

    @contextmanager
    def assignment_report_collector_lock(self, job_name: str, assignment_id: str) -> Iterator[bool]:
        """Claim one collector for an exact Job and Assignment scope."""
        validate_job_name(job_name)
        validate_assignment_id(assignment_id)
        with self._named_process_lock(
            job_name,
            f"reports-{assignment_id}",
            blocking=False,
        ) as acquired:
            yield acquired

    @contextmanager
    def _named_process_lock(
        self, name: str, purpose: str, *, blocking: bool = True
    ) -> Iterator[bool]:
        locks_path = self.database_path.parent / "locks"
        locks_path.mkdir(parents=True, exist_ok=True, mode=0o700)
        descriptor = os.open(
            locks_path / f"{name}.{purpose}.lock",
            os.O_RDWR | os.O_CREAT | getattr(os, "O_NOFOLLOW", 0),
            0o600,
        )
        acquired = False
        try:
            os.fchmod(descriptor, 0o600)
            operation = fcntl.LOCK_EX if blocking else fcntl.LOCK_EX | fcntl.LOCK_NB
            try:
                fcntl.flock(descriptor, operation)
                acquired = True
            except BlockingIOError:
                pass
            yield acquired
        finally:
            if acquired:
                fcntl.flock(descriptor, fcntl.LOCK_UN)
            os.close(descriptor)

    def _migrate(self) -> None:
        self._create_workers_tables()
        self._create_independent_jobs_tables()
        self._connection.commit()

    def _create_workers_tables(self) -> None:
        self._connection.execute(
            """
            CREATE TABLE IF NOT EXISTS workers (
                id INTEGER PRIMARY KEY,
                name TEXT NOT NULL UNIQUE,
                harness_type TEXT NOT NULL,
                provenance TEXT NOT NULL,
                lifecycle_policy TEXT NOT NULL,
                status TEXT NOT NULL,
                error TEXT,
                current_room_id TEXT,
                created_at TEXT NOT NULL,
                updated_at TEXT NOT NULL
            )
            """
        )
        self._connection.execute(
            """
            CREATE TABLE IF NOT EXISTS rooms (
                id TEXT PRIMARY KEY,
                worker_name TEXT NOT NULL,
                room_type TEXT NOT NULL,
                provider_id TEXT NOT NULL,
                workspace TEXT NOT NULL,
                status TEXT NOT NULL,
                error TEXT,
                metadata TEXT NOT NULL DEFAULT '{}',
                created_at TEXT NOT NULL,
                updated_at TEXT NOT NULL
            )
            """
        )
        self._connection.execute(
            """
            CREATE TABLE IF NOT EXISTS worker_presence (
                attachment_id TEXT PRIMARY KEY,
                worker_name TEXT NOT NULL UNIQUE,
                room_id TEXT NOT NULL,
                workspace TEXT NOT NULL,
                attached_at TEXT NOT NULL
            )
            """
        )
        self._connection.execute(
            """
            CREATE TABLE IF NOT EXISTS worker_conversations (
                id TEXT PRIMARY KEY,
                worker_name TEXT NOT NULL,
                kind TEXT NOT NULL,
                native_conversation_id TEXT,
                model TEXT NOT NULL,
                reasoning_effort TEXT NOT NULL,
                status TEXT NOT NULL,
                error TEXT,
                created_at TEXT NOT NULL,
                updated_at TEXT NOT NULL,
                UNIQUE (worker_name, kind)
            )
            """
        )
        self._connection.execute(
            """
            CREATE TABLE IF NOT EXISTS worker_messages (
                id TEXT PRIMARY KEY,
                conversation_id TEXT NOT NULL,
                worker_name TEXT NOT NULL,
                sequence INTEGER NOT NULL,
                text TEXT NOT NULL,
                model TEXT NOT NULL,
                reasoning_effort TEXT NOT NULL,
                created_at TEXT NOT NULL,
                UNIQUE (conversation_id, sequence)
            )
            """
        )
        self._connection.execute(
            """
            CREATE TABLE IF NOT EXISTS worker_turns (
                id INTEGER PRIMARY KEY,
                conversation_id TEXT NOT NULL,
                worker_name TEXT NOT NULL,
                message_id TEXT NOT NULL UNIQUE,
                status TEXT NOT NULL,
                exit_code INTEGER,
                output_path TEXT NOT NULL,
                input_digest TEXT NOT NULL,
                native_turn_baseline_id TEXT,
                native_turn_id TEXT,
                phase TEXT NOT NULL,
                error TEXT,
                started_at TEXT NOT NULL,
                finished_at TEXT
            )
            """
        )

    def _create_independent_jobs_tables(self) -> None:
        self._connection.execute(
            """
            CREATE TABLE IF NOT EXISTS jobs (
                id INTEGER PRIMARY KEY,
                name TEXT NOT NULL UNIQUE,
                status TEXT NOT NULL,
                goal_version INTEGER NOT NULL,
                goal TEXT NOT NULL,
                created_at TEXT NOT NULL,
                updated_at TEXT NOT NULL
            )
            """
        )
        self._connection.execute(
            """
            CREATE TABLE IF NOT EXISTS assignments (
                id TEXT PRIMARY KEY,
                job_name TEXT NOT NULL,
                worker_name TEXT NOT NULL,
                generation INTEGER NOT NULL,
                status TEXT NOT NULL,
                room_id TEXT NOT NULL,
                workspace TEXT NOT NULL,
                started_at TEXT NOT NULL,
                ended_at TEXT,
                UNIQUE (job_name, generation)
            )
            """
        )
        self._connection.execute(
            """
            CREATE UNIQUE INDEX IF NOT EXISTS assignments_one_unended_worker
            ON assignments(worker_name) WHERE status != 'ended'
            """
        )
        self._connection.execute(
            """
            CREATE TABLE IF NOT EXISTS job_conversations (
                id TEXT PRIMARY KEY,
                job_name TEXT NOT NULL UNIQUE,
                native_conversation_id TEXT,
                model TEXT NOT NULL,
                reasoning_effort TEXT NOT NULL,
                status TEXT NOT NULL,
                error TEXT,
                created_at TEXT NOT NULL,
                updated_at TEXT NOT NULL
            )
            """
        )
        self._connection.execute(
            """
            CREATE TABLE IF NOT EXISTS job_inputs (
                id TEXT PRIMARY KEY,
                conversation_id TEXT NOT NULL,
                job_name TEXT NOT NULL,
                sequence INTEGER NOT NULL,
                kind TEXT NOT NULL,
                goal_version INTEGER,
                text TEXT NOT NULL,
                model TEXT NOT NULL,
                reasoning_effort TEXT NOT NULL,
                created_at TEXT NOT NULL,
                UNIQUE (conversation_id, sequence)
            )
            """
        )
        self._connection.execute(
            """
            CREATE TABLE IF NOT EXISTS job_turns (
                id INTEGER PRIMARY KEY,
                conversation_id TEXT NOT NULL,
                job_name TEXT NOT NULL,
                input_id TEXT NOT NULL UNIQUE,
                status TEXT NOT NULL,
                exit_code INTEGER,
                output_path TEXT NOT NULL,
                input_digest TEXT NOT NULL,
                native_turn_baseline_id TEXT,
                native_turn_id TEXT,
                phase TEXT NOT NULL,
                error TEXT,
                started_at TEXT NOT NULL,
                finished_at TEXT
            )
            """
        )


def _worker_presence_from_row(row: sqlite3.Row) -> WorkerPresence:
    return WorkerPresence(
        attachment_id=row["attachment_id"],
        worker_name=row["worker_name"],
        room_id=row["room_id"],
        workspace=row["workspace"],
        attached_at=row["attached_at"],
    )


def _worker_from_row(row: sqlite3.Row, native_conversation_id: str | None) -> Worker:
    return Worker(
        id=row["id"],
        name=row["name"],
        harness_type=row["harness_type"],
        provenance=row["provenance"],
        lifecycle_policy=row["lifecycle_policy"],
        status=row["status"],
        error=row["error"],
        current_room_id=row["current_room_id"],
        general_conversation_id=native_conversation_id,
        created_at=row["created_at"],
        updated_at=row["updated_at"],
    )


def _room_from_row(row: sqlite3.Row) -> Room:
    return Room(
        id=row["id"],
        worker_name=row["worker_name"],
        room_type=row["room_type"],
        provider_id=row["provider_id"],
        workspace=row["workspace"],
        status=row["status"],
        error=row["error"],
        metadata=json.loads(row["metadata"] or "{}"),
        created_at=row["created_at"],
        updated_at=row["updated_at"],
    )


def _worker_conversation_from_row(row: sqlite3.Row) -> WorkerConversation:
    return WorkerConversation(
        id=row["id"],
        worker_name=row["worker_name"],
        kind=row["kind"],
        native_conversation_id=row["native_conversation_id"],
        model=row["model"],
        reasoning_effort=row["reasoning_effort"],
        status=row["status"],
        error=row["error"],
        created_at=row["created_at"],
        updated_at=row["updated_at"],
    )


def _worker_message_from_row(row: sqlite3.Row) -> WorkerMessage:
    return WorkerMessage(
        id=row["id"],
        conversation_id=row["conversation_id"],
        worker_name=row["worker_name"],
        sequence=row["sequence"],
        text=row["text"],
        model=row["model"],
        reasoning_effort=row["reasoning_effort"],
        created_at=row["created_at"],
    )


def _worker_turn_from_row(row: sqlite3.Row) -> WorkerTurn:
    return WorkerTurn(
        id=row["id"],
        conversation_id=row["conversation_id"],
        worker_name=row["worker_name"],
        message_id=row["message_id"],
        status=row["status"],
        exit_code=row["exit_code"],
        output_path=row["output_path"],
        input_digest=row["input_digest"],
        native_turn_baseline_id=row["native_turn_baseline_id"],
        native_turn_id=row["native_turn_id"],
        phase=row["phase"],
        error=row["error"],
        started_at=row["started_at"],
        finished_at=row["finished_at"],
    )


def _job_from_row(row: sqlite3.Row) -> Job:
    return Job(
        id=row["id"],
        name=row["name"],
        status=row["status"],
        goal_version=row["goal_version"],
        goal=row["goal"],
        created_at=row["created_at"],
        updated_at=row["updated_at"],
    )


def _assignment_from_row(row: sqlite3.Row) -> Assignment:
    return Assignment(
        id=row["id"],
        job_name=row["job_name"],
        worker_name=row["worker_name"],
        generation=row["generation"],
        status=row["status"],
        room_id=row["room_id"],
        workspace=row["workspace"],
        started_at=row["started_at"],
        ended_at=row["ended_at"],
    )


def _job_conversation_from_row(row: sqlite3.Row) -> JobConversation:
    return JobConversation(
        id=row["id"],
        job_name=row["job_name"],
        native_conversation_id=row["native_conversation_id"],
        model=row["model"],
        reasoning_effort=row["reasoning_effort"],
        status=row["status"],
        error=row["error"],
        created_at=row["created_at"],
        updated_at=row["updated_at"],
    )


def _job_input_from_row(row: sqlite3.Row) -> JobInput:
    return JobInput(
        id=row["id"],
        conversation_id=row["conversation_id"],
        job_name=row["job_name"],
        sequence=row["sequence"],
        kind=row["kind"],
        goal_version=row["goal_version"],
        text=row["text"],
        model=row["model"],
        reasoning_effort=row["reasoning_effort"],
        created_at=row["created_at"],
    )


def _job_turn_from_row(row: sqlite3.Row) -> JobTurn:
    return JobTurn(
        id=row["id"],
        conversation_id=row["conversation_id"],
        job_name=row["job_name"],
        input_id=row["input_id"],
        status=row["status"],
        exit_code=row["exit_code"],
        output_path=row["output_path"],
        input_digest=row["input_digest"],
        native_turn_baseline_id=row["native_turn_baseline_id"],
        native_turn_id=row["native_turn_id"],
        phase=row["phase"],
        error=row["error"],
        started_at=row["started_at"],
        finished_at=row["finished_at"],
    )


def _now() -> str:
    return datetime.now(UTC).isoformat(timespec="microseconds")
