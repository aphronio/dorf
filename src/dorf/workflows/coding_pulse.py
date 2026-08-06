"""Outcome-first inspection for one coding Job."""

from __future__ import annotations

import json
from dataclasses import dataclass

from dorf.runtime import JobInspection, TimelineEvent

from .coding import AFK_TERMINAL_JOB_STATUSES
from .coding_store import CodingCommandRun, CodingJob, CodingStore

_GOAL_SUMMARY_LIMIT = 160


@dataclass(frozen=True)
class PulseDelta:
    summary: str
    updated_at: str
    source: str
    provenance: str


@dataclass(frozen=True)
class PulseActivity:
    status: str
    detail: str
    observed_at: str
    source: str
    provenance: str
    claim_support: str


@dataclass(frozen=True)
class PulseWorkerClaim:
    summary: str
    recorded_at: str
    source: str
    provenance: str
    assignment_id: str | None


@dataclass(frozen=True)
class PulseAttention:
    state: str
    reason: str


@dataclass(frozen=True)
class PulseLifecycle:
    state: str
    updated_at: str
    source: str
    provenance: str


@dataclass(frozen=True)
class PulseRoomAvailability:
    status: str
    detail: str | None
    observed_at: str
    source: str
    provenance: str


@dataclass(frozen=True)
class CodingJobPulse:
    job: str
    goal: str
    goal_summary: str
    goal_version: int
    outcome_stage: str
    lifecycle: PulseLifecycle
    room_availability: PulseRoomAvailability
    latest_delta: PulseDelta
    updated_at: str
    evidence_count: int
    observed_activity: PulseActivity
    worker_claim: PulseWorkerClaim | None
    attention: PulseAttention


def build_coding_job_pulse(
    store: CodingStore,
    inspection: JobInspection,
) -> CodingJobPulse:
    """Compose runtime, workflow, and accepted-document authority without process identity."""
    coding_job = store.get_coding_job(inspection.job.name)
    if coding_job is None:
        raise RuntimeError(f"Coding Job not found: {inspection.job.name}")
    events = store.documents.list_events(inspection.job.name)
    workflow_terminal = (
        coding_job.status if coding_job.status in AFK_TERMINAL_JOB_STATUSES else None
    )
    if workflow_terminal is not None:
        terminal = workflow_terminal
        terminal_source = "workflow"
        terminal_updated_at = coding_job.updated_at
    elif inspection.job.status == "ended":
        terminal = "ended"
        terminal_source = "runtime"
        terminal_updated_at = inspection.job.updated_at
    else:
        terminal = None
        terminal_source = None
        terminal_updated_at = None

    outcome_stage = _outcome_stage(coding_job.status, coding_job.metadata, terminal)
    latest_delta = _latest_delta(
        coding_job,
        events,
        terminal=terminal,
        terminal_source=terminal_source,
        terminal_updated_at=terminal_updated_at,
    )
    latest_claim_event = next(
        (
            event
            for event in reversed(events)
            if event.source == "worker" and event.provenance == "claim"
        ),
        None,
    )
    worker_claim = _worker_claim(latest_claim_event)
    active_command = next(
        (
            run
            for run in store.list_command_runs(inspection.job.name)
            if run.status == "running" and run.kind != "afk"
        ),
        None,
    )
    activity = _observed_activity(
        inspection,
        active_command=active_command,
        worker_claim=worker_claim,
        terminal=terminal,
        terminal_source=terminal_source,
    )
    attention = _attention(
        coding_job,
        inspection,
        outcome_stage=outcome_stage,
        terminal=terminal,
    )
    timestamps = [
        inspection.job.updated_at,
        coding_job.updated_at,
        latest_delta.updated_at,
        *(event.recorded_at for event in events),
    ]
    if inspection.latest_turn is not None:
        timestamps.append(inspection.latest_turn.finished_at or inspection.latest_turn.started_at)
    if active_command is not None:
        timestamps.append(active_command.started_at)
    return CodingJobPulse(
        job=inspection.job.name,
        goal=inspection.job.goal,
        goal_summary=_goal_summary(coding_job, inspection.job.goal),
        goal_version=inspection.job.goal_version,
        outcome_stage=outcome_stage,
        lifecycle=PulseLifecycle(
            inspection.job.status,
            inspection.job.updated_at,
            "runtime",
            "fact",
        ),
        room_availability=_room_availability(inspection),
        latest_delta=latest_delta,
        updated_at=max(timestamps),
        evidence_count=sum(len(event.artifacts) for event in events),
        observed_activity=activity,
        worker_claim=worker_claim,
        attention=attention,
    )


def _outcome_stage(status: str, metadata: dict[str, str], terminal: str | None) -> str:
    if terminal is not None:
        return terminal
    if status in {"ready", "needs-human"}:
        return status
    return metadata.get("afk_stage") or status


def _latest_delta(
    coding_job: CodingJob,
    events: list[TimelineEvent],
    *,
    terminal: str | None,
    terminal_source: str | None,
    terminal_updated_at: str | None,
) -> PulseDelta:
    if terminal is not None:
        if terminal_source == "runtime":
            return PulseDelta(
                f"Job lifecycle is {terminal}",
                terminal_updated_at or coding_job.updated_at,
                "runtime",
                "fact",
            )
        return PulseDelta(
            f"Coding outcome is {terminal}",
            terminal_updated_at or coding_job.updated_at,
            "workflow",
            "fact",
        )
    if coding_job.status in {"ready", "needs-human"}:
        matching_outcome = (
            coding_job.metadata.get("afk_outcome")
            if coding_job.metadata.get("afk_stage") == coding_job.status
            else None
        )
        return PulseDelta(
            matching_outcome or f"Coding outcome is {coding_job.status}",
            coding_job.updated_at,
            "workflow",
            "fact",
        )
    outcome = coding_job.metadata.get("afk_outcome")
    candidates = [
        PulseDelta(
            outcome or f"Coding state is {coding_job.status}",
            coding_job.updated_at,
            "workflow",
            "fact",
        )
    ]
    candidates.extend(
        PulseDelta(event.summary, event.recorded_at, event.source, event.provenance)
        for event in events
        if event.kind != "input-admitted"
    )
    return max(candidates, key=lambda delta: delta.updated_at)


def _goal_summary(coding_job: CodingJob, pinned_goal: str) -> str:
    candidate = coding_job.task.strip() or next(
        (line.strip() for line in pinned_goal.splitlines() if line.strip()),
        "Goal not recorded",
    )
    compact = " ".join(candidate.split())
    if len(compact) <= _GOAL_SUMMARY_LIMIT:
        return compact
    return f"{compact[: _GOAL_SUMMARY_LIMIT - 1].rstrip()}…"


def _room_availability(inspection: JobInspection) -> PulseRoomAvailability:
    detail = inspection.room_observation_error
    if detail is not None:
        for identity in (inspection.room.provider_id, inspection.room.id):
            if identity:
                detail = detail.replace(identity, "Room")
    return PulseRoomAvailability(
        status=inspection.room_observation,
        detail=detail,
        observed_at=inspection.observed_at,
        source="runtime",
        provenance="fact",
    )


def _worker_claim(event: TimelineEvent | None) -> PulseWorkerClaim | None:
    if event is None:
        return None
    return PulseWorkerClaim(
        summary=event.summary,
        recorded_at=event.recorded_at,
        source=event.source,
        provenance=event.provenance,
        assignment_id=event.related.get("assignment"),
    )


def _observed_activity(
    inspection: JobInspection,
    *,
    active_command: CodingCommandRun | None,
    worker_claim: PulseWorkerClaim | None,
    terminal: str | None,
    terminal_source: str | None,
) -> PulseActivity:
    if terminal is not None:
        return PulseActivity(
            "settled",
            f"No activity is current because the Job is terminal: {terminal}",
            inspection.observed_at,
            terminal_source or "runtime",
            "fact",
            "superseded" if worker_claim is not None else "not-applicable",
        )
    turn = inspection.latest_turn
    if turn is not None and turn.status == "running":
        room_available = inspection.room_observation == "available"
        support = (
            "consistent"
            if room_available
            and worker_claim is not None
            and worker_claim.assignment_id == inspection.assignment.id
            else "unconfirmed"
            if worker_claim is not None
            else "not-applicable"
        )
        return PulseActivity(
            "active" if room_available else "unconfirmed",
            (
                f"Job input is recorded running ({turn.phase})"
                if room_available
                else (
                    "Job input remains recorded running while its Room is unavailable "
                    f"({turn.phase})"
                )
            ),
            inspection.observed_at,
            "runtime",
            "fact",
            support,
        )
    if active_command is not None:
        return PulseActivity(
            "unconfirmed",
            f"Workflow command {active_command.kind} is recorded running",
            inspection.observed_at,
            "workflow",
            "fact",
            "unconfirmed" if worker_claim is not None else "not-applicable",
        )
    detail = "No Job turn or workflow command is active"
    if turn is not None:
        detail = f"Latest Job input is {turn.status} ({turn.phase})"
    return PulseActivity(
        "quiet",
        detail,
        inspection.observed_at,
        "runtime",
        "fact",
        "unconfirmed" if worker_claim is not None else "not-applicable",
    )


def _attention(
    coding_job: CodingJob,
    inspection: JobInspection,
    *,
    outcome_stage: str,
    terminal: str | None,
) -> PulseAttention:
    if terminal is not None:
        return PulseAttention("none", f"Job is terminal: {terminal}")
    if raw := coding_job.metadata.get("diff_verifier_attention"):
        try:
            verifier = json.loads(raw)
        except json.JSONDecodeError:
            verifier = {}
        if verifier.get("status"):
            return PulseAttention(
                str(verifier["status"]),
                f"DeepSeek diff advisory decision {verifier.get('id', 'is retained')}",
            )
    if outcome_stage == "needs-human":
        return PulseAttention("needs-human", "Coding workflow requires a human decision")
    turn = inspection.latest_turn
    if turn is not None and turn.status in {"failed", "interrupted", "recovery-required"}:
        return PulseAttention("needs-human", f"Latest Job input is {turn.status}")
    return PulseAttention("quiet", "No human decision is recorded")
