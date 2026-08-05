"""Acceptance compilation and user-facing proof for the coding workflow."""

from __future__ import annotations

import json
import re
from collections.abc import Callable
from dataclasses import dataclass

from dorf.repo_contract import RepoContract
from dorf.runtime import JobArtifact, JobBinding, TimelineEvent

from .coding_store import AcceptanceItem, CodingCommandRun, CodingJob, CodingStore

_CHECKBOX = re.compile(r"^\s*-\s*\[[ xX]\]\s+(.+?)\s*$")
_HEADING = re.compile(r"^\s*##+\s+(.+?)\s*$")
REVIEW_NO_FINDINGS_SENTINEL = "DORF_REVIEW_NO_FINDINGS"
_CODEX_TELEMETRY_DELIMITER = "tokens used"
_CODEX_TOKEN_COUNT = re.compile(r"^(?:0|[1-9]\d*|[1-9]\d{0,2}(?:,\d{3})+)$")
_VERIFICATION_COMMANDS = ("check", "smoke")


@dataclass(frozen=True)
class DossierArtifact:
    ref: str
    name: str
    digest: str
    size: int
    media_type: str
    event_id: str
    source: str
    provenance: str


@dataclass(frozen=True)
class ProofEvidence:
    evidence_id: str
    summary: str
    source: str
    provenance: str
    commit_sha: str | None
    command_run_id: int | None
    command: str | None
    turn_id: int | None
    assignment_id: str | None
    room_id: str | None
    environment_type: str
    artifacts: tuple[DossierArtifact, ...]


@dataclass(frozen=True)
class AcceptanceResult:
    key: str
    text: str
    source: str
    status: str
    reason: str
    evidence: tuple[ProofEvidence, ...]


@dataclass(frozen=True)
class EnvironmentProvenance:
    environment_type: str
    room_id: str
    provider_id: str
    assignment_id: str
    worker: str
    harness: str
    metadata: tuple[tuple[str, str], ...]


@dataclass(frozen=True)
class CleanupState:
    job: str
    assignment: str
    room: str


@dataclass(frozen=True)
class ProofDossier:
    job: str
    outcome: str
    verdict: str
    commit_sha: str
    checklist_state: str
    checklist_revision: int
    acceptance: tuple[AcceptanceResult, ...]
    environment: EnvironmentProvenance
    checks: tuple[ProofEvidence, ...]
    independent_review: tuple[ProofEvidence, ...]
    assumptions_and_claims: tuple[ProofEvidence, ...]
    unresolved_risks: tuple[str, ...]
    relevant_artifacts: tuple[DossierArtifact, ...]
    cleanup: CleanupState


def compile_acceptance_checklist(
    goal: str,
    contract: RepoContract,
    *,
    review_commands: dict[str, str] | None = None,
) -> tuple[AcceptanceItem, ...]:
    """Compile concise issue criteria plus concrete repository verification obligations."""
    criteria = _acceptance_criteria(goal)
    enabled_reviewers = tuple(
        name
        for name, agent in (contract.review.agents.items() if contract.review else ())
        if agent.enabled
    )
    if enabled_reviewers:
        missing = [
            name
            for name in enabled_reviewers
            if not review_commands or not review_commands.get(name)
        ]
        if missing:
            raise ValueError(
                "Acceptance compilation requires exact rendered review commands: "
                + ", ".join(missing)
            )
    if enabled_reviewers:
        issue_verifier, issue_verifier_ref = "review", "*"
    elif "check" in contract.commands:
        issue_verifier, issue_verifier_ref = "command", "check"
    elif "smoke" in contract.commands:
        issue_verifier, issue_verifier_ref = "command", "smoke"
    else:
        issue_verifier, issue_verifier_ref = None, ""
    items = [
        AcceptanceItem(
            key=f"issue-{position}",
            text=text,
            source="issue",
            verifier=issue_verifier,
            verifier_ref=issue_verifier_ref,
            verifier_command=(
                contract.commands[issue_verifier_ref]
                if issue_verifier == "command"
                else _review_command_pin("*", review_commands or {})
            ),
        )
        for position, text in enumerate(criteria, start=1)
        if issue_verifier is not None
    ]
    if not items and issue_verifier is not None:
        summary = next((line.strip() for line in goal.splitlines() if line.strip()), "Pinned goal")
        items.append(
            AcceptanceItem(
                key="goal-1",
                text=summary,
                source="goal",
                verifier=issue_verifier,
                verifier_ref=issue_verifier_ref,
                verifier_command=(
                    contract.commands[issue_verifier_ref]
                    if issue_verifier == "command"
                    else _review_command_pin("*", review_commands or {})
                ),
            )
        )
    for name in _VERIFICATION_COMMANDS:
        if command := contract.commands.get(name):
            items.append(
                AcceptanceItem(
                    key=f"repo-{name}",
                    text=f"Repository {name} passes: {command}",
                    source="contract",
                    verifier="command",
                    verifier_ref=name,
                    verifier_command=command,
                )
            )
    for position, name in enumerate(enabled_reviewers, start=1):
        items.append(
            AcceptanceItem(
                key=f"review-{position}-{_key_part(name)}",
                text=f"Independent review by {name} reports no findings",
                source="contract",
                verifier="review",
                verifier_ref=name,
                verifier_command=(review_commands or {})[name],
            )
        )
    return tuple(items)


def build_proof_dossier(
    store: CodingStore,
    job: CodingJob,
    binding: JobBinding,
    *,
    commit_sha: str,
) -> ProofDossier:
    """Project retained workflow/runtime records into one commit-scoped assessment."""
    runs = store.list_command_runs(job.job_name)
    artifacts = store.documents.list_artifacts(job.job_name, job_id=binding.job.id)
    artifacts_by_event: dict[str, tuple[DossierArtifact, ...]] = {}
    for artifact in artifacts:
        artifacts_by_event.setdefault(artifact.event_id, ())
        artifacts_by_event[artifact.event_id] += (_dossier_artifact(artifact),)
    events = store.documents.list_events(job.job_name)
    command_events = {
        event.related.get("run"): event
        for event in events
        if event.source == "workflow"
        and event.provenance == "fact"
        and event.kind == "command-result"
    }
    clean_review_run_ids = frozenset(
        int(run_id)
        for event in events
        if event.source == "workflow"
        and event.provenance == "fact"
        and event.kind == "review-verdict"
        and event.related.get("verdict") == "no-findings"
        and event.related.get("commit") == commit_sha
        and (run_id := event.related.get("run")) is not None
        and run_id.isdigit()
    )
    run_evidence = {
        run.id: _command_evidence(
            run,
            binding=binding,
            event=command_events.get(str(run.id)),
            artifacts_by_event=artifacts_by_event,
        )
        for run in runs
    }
    checklist = store.get_acceptance_checklist(job.job_name)
    acceptance = tuple(
        _evaluate_acceptance_item(
            item,
            runs=runs,
            run_evidence=run_evidence,
            commit_sha=commit_sha,
            expected_reviewers=tuple(
                candidate.verifier_ref
                for candidate in checklist.items
                if candidate.key.startswith("review-") and candidate.verifier_ref
            ),
            clean_review_run_ids=clean_review_run_ids,
        )
        for item in (checklist.items if checklist else ())
    )
    checks = tuple(
        run_evidence[run.id]
        for run in _compact_runs(runs, commit_sha, lambda run: run.kind in _VERIFICATION_COMMANDS)
    )
    reviews = tuple(
        run_evidence[run.id]
        for run in _compact_runs(runs, commit_sha, lambda run: run.kind.startswith("review:"))
    )
    claim_events = [
        event
        for event in events
        if event.source == "worker" and event.provenance == "claim"
    ][-5:]
    claims = tuple(
        _claim_evidence(
            store,
            event,
            binding=binding,
            artifacts=artifacts_by_event.get(event.id, ()),
        )
        for event in claim_events
    )
    unproven_acceptance = [
        result for result in acceptance if result.status != "proven"
    ]
    risks = []
    if unproven_acceptance:
        risks.append(
            f"{len(unproven_acceptance)} of {len(acceptance)} acceptance items remain "
            f"unproven at {commit_sha}"
        )
    if checklist is None:
        risks.append("Acceptance checklist was not compiled at admission")
    elif not checklist.items:
        risks.append(
            "Repository supplied no machine-verifiable acceptance items; human GitHub "
            "acceptance remains required"
        )
    image_fingerprint = binding.metadata.get("image_fingerprint")
    if (
        image_fingerprint is None
        or re.fullmatch(r"[0-9a-f]{64}", image_fingerprint) is None
    ):
        risks.append(
            "Room provenance does not retain a validated immutable image fingerprint"
        )
    if any(claim.commit_sha is None for claim in claims):
        risks.append("Unpinned Worker claim cannot support acceptance for this commit")
    if any(claim.turn_id is None for claim in claims):
        risks.append("Worker claim was accepted without an exact Job turn association")
    if len(claim_events) < sum(
        event.source == "worker" and event.provenance == "claim" for event in events
    ):
        risks.append("Earlier Worker claims remain available in the timeline audit layer")
    all_proven = all(item.status == "proven" for item in acceptance)
    proof_commit = job.metadata.get("proof_commit")
    readiness_proven = proof_commit == commit_sha
    if job.status == "ready" and not readiness_proven:
        risks.append(
            f"Workflow readiness is pinned to {proof_commit or 'no commit'}, not {commit_sha}"
        )
    verdict = (
        "ready"
        if job.status == "ready" and all_proven and readiness_proven
        else "not ready"
    )
    acceptance_evidence = tuple(
        evidence for result in acceptance for evidence in result.evidence
    )
    return ProofDossier(
        job=job.job_name,
        outcome=job.status,
        verdict=verdict,
        commit_sha=commit_sha,
        checklist_state=checklist.state if checklist else "missing",
        checklist_revision=checklist.revision if checklist else 0,
        acceptance=acceptance,
        environment=EnvironmentProvenance(
            environment_type=binding.environment_type,
            room_id=binding.room.id,
            provider_id=binding.room.provider_id,
            assignment_id=binding.assignment.id,
            worker=binding.worker.name,
            harness=binding.worker.harness_type,
            metadata=tuple(
                sorted((str(key), str(value)) for key, value in binding.metadata.items())
            ),
        ),
        checks=checks,
        independent_review=reviews,
        assumptions_and_claims=claims,
        unresolved_risks=tuple(dict.fromkeys(risks)),
        relevant_artifacts=_relevant_artifacts(
            (*acceptance_evidence, *checks, *reviews, *claims)
        ),
        cleanup=CleanupState(
            job=binding.job.status,
            assignment=binding.assignment.status,
            room=binding.room.status,
        ),
    )


def acceptance_is_proven(dossier: ProofDossier) -> bool:
    return all(item.status == "proven" for item in dossier.acceptance)


def render_proof_dossier(dossier: ProofDossier) -> str:
    """Render the compact default; stable references expose the deeper audit layer."""
    lines = [
        f"# Dorf proof dossier · {dossier.job}",
        "",
        "## Outcome / verdict",
        "",
        f"{dossier.outcome} · **{dossier.verdict}**",
        "",
        "## Exact commit",
        "",
        f"`{dossier.commit_sha}`",
        "",
        "## Acceptance status",
        "",
        f"Checklist {dossier.checklist_state}, revision {dossier.checklist_revision}.",
    ]
    if dossier.acceptance:
        for item in dossier.acceptance:
            mark = "x" if item.status == "proven" else " "
            refs = ", ".join(evidence.evidence_id for evidence in item.evidence)
            suffix = (
                f" — {refs}"
                if refs
                else f" — {_compact_acceptance_reason(item)}"
            )
            lines.append(f"- [{mark}] {item.text}{suffix}")
    else:
        lines.append("No machine-verifiable acceptance items were admitted.")
    environment = dossier.environment
    lines.extend(
        [
            "",
            "## Environment / image provenance",
            "",
            f"{environment.environment_type} · Room `{environment.room_id}` · "
            f"Assignment `{environment.assignment_id}` · Worker `{environment.worker}` "
            f"({environment.harness})",
        ]
    )
    if environment.metadata:
        lines.append(" · ".join(f"{key}=`{value}`" for key, value in environment.metadata))
    _render_evidence_section(lines, "Checks", dossier.checks)
    _render_evidence_section(lines, "Independent review", dossier.independent_review)
    _render_evidence_section(
        lines,
        "Assumptions and Worker claims",
        dossier.assumptions_and_claims,
    )
    lines.extend(["", "## Unresolved risks", ""])
    if dossier.unresolved_risks:
        lines.extend(f"- {risk}" for risk in dossier.unresolved_risks)
    else:
        lines.append("None recorded.")
    lines.extend(["", "## Relevant artifacts", ""])
    if dossier.relevant_artifacts:
        for artifact in dossier.relevant_artifacts:
            lines.append(
                f"- `{artifact.ref}` · {artifact.name} · `{artifact.digest}` · "
                f"{artifact.source} {artifact.provenance}"
            )
    else:
        lines.append("None retained.")
    lines.extend(
        [
            "",
            "## Cleanup state",
            "",
            f"Job {dossier.cleanup.job} · Assignment {dossier.cleanup.assignment} · "
            f"Room {dossier.cleanup.room}",
            "",
            "## Audit layer",
            "",
            f"`dorf runs {dossier.job}` · `dorf job inspect {dossier.job} --timeline` · "
            f"`dorf job artifact list {dossier.job}`",
        ]
    )
    return "\n".join(lines)


def _compact_acceptance_reason(item: AcceptanceResult) -> str:
    repeated_text = f": {item.text}"
    if item.reason.endswith(repeated_text):
        return item.reason[: -len(repeated_text)]
    return item.reason


def _acceptance_criteria(goal: str) -> list[str]:
    inside = False
    criteria: list[str] = []
    for line in goal.splitlines():
        heading = _HEADING.match(line)
        if heading:
            inside = heading.group(1).strip().casefold() == "acceptance criteria"
            continue
        if inside and (match := _CHECKBOX.match(line)):
            criteria.append(" ".join(match.group(1).split()))
    return criteria


def _key_part(value: str) -> str:
    normalized = re.sub(r"[^a-z0-9]+", "-", value.casefold()).strip("-")
    return normalized or "reviewer"


def _evaluate_acceptance_item(
    item: AcceptanceItem,
    *,
    runs: list[CodingCommandRun],
    run_evidence: dict[int, ProofEvidence],
    commit_sha: str,
    expected_reviewers: tuple[str, ...],
    clean_review_run_ids: frozenset[int],
) -> AcceptanceResult:
    if item.verifier == "manual":
        return AcceptanceResult(
            item.key,
            item.text,
            item.source,
            "unproven",
            f"Manual acceptance remains unresolved: {item.text}",
            (),
        )
    if item.verifier == "command":
        candidates = [
            run
            for run in runs
            if run.kind == item.verifier_ref
            and run.command == item.verifier_command
        ]
        latest = next(
            (run for run in candidates if _observed_at_commit(run, commit_sha)),
            None,
        )
        matching = (
            [latest]
            if latest is not None and _successful_at_commit(latest, commit_sha)
            else []
        )
    else:
        expected_commands = _review_commands_for_item(item, expected_reviewers)
        reviewer_names = tuple(expected_commands)
        matching = []
        for reviewer in reviewer_names:
            latest = next(
                (
                    run
                    for run in runs
                    if run.kind == f"review:{reviewer}"
                    and run.command == expected_commands[reviewer]
                    and _observed_at_commit(run, commit_sha)
                ),
                None,
            )
            if (
                latest is not None
                and _successful_at_commit(latest, commit_sha)
                and latest.id in clean_review_run_ids
            ):
                matching.append(latest)
        candidates = [
            run
            for run in runs
            if run.kind in {f"review:{reviewer}" for reviewer in reviewer_names}
            and run.command == expected_commands.get(run.kind.removeprefix("review:"))
        ]
        if len(matching) != len(reviewer_names):
            matching = []
    if matching:
        return AcceptanceResult(
            item.key,
            item.text,
            item.source,
            "proven",
            "Observed workflow evidence passes at the exact dossier commit",
            tuple(run_evidence[run.id] for run in matching),
        )
    stale = any(
        run.status == "succeeded"
        and run.exit_code == 0
        and run.finished_at is not None
        and run.git_commit_before == run.git_commit_after
        and run.git_commit_after is not None
        and run.git_commit_after != commit_sha
        for run in candidates
    )
    reason = (
        f"Only evidence for an older commit exists: {item.text}"
        if stale
        else f"No passing observed evidence at {commit_sha}: {item.text}"
    )
    return AcceptanceResult(item.key, item.text, item.source, "unproven", reason, ())


def _successful_at_commit(run: CodingCommandRun, commit_sha: str) -> bool:
    return (
        run.status == "succeeded"
        and run.exit_code == 0
        and run.finished_at is not None
        and _observed_at_commit(run, commit_sha)
    )


def _observed_at_commit(run: CodingCommandRun, commit_sha: str) -> bool:
    return run.git_commit_before == commit_sha and run.git_commit_after == commit_sha


def _review_command_pin(verifier_ref: str, commands: dict[str, str]) -> str:
    if verifier_ref == "*":
        return json.dumps(commands, sort_keys=True, separators=(",", ":"))
    return commands[verifier_ref]


def _review_commands_for_item(
    item: AcceptanceItem,
    expected_reviewers: tuple[str, ...],
) -> dict[str, str]:
    if item.verifier_ref != "*":
        return {item.verifier_ref: item.verifier_command}
    try:
        decoded = json.loads(item.verifier_command)
    except (json.JSONDecodeError, TypeError):
        return {}
    if not isinstance(decoded, dict):
        return {}
    commands = {
        reviewer: command
        for reviewer in expected_reviewers
        if isinstance((command := decoded.get(reviewer)), str) and command
    }
    return commands if len(commands) == len(expected_reviewers) else {}


def _compact_runs(
    runs: list[CodingCommandRun],
    commit_sha: str,
    include: Callable[[CodingCommandRun], bool],
) -> tuple[CodingCommandRun, ...]:
    """Keep one useful run per check/reviewer while exact logs remain in the audit layer."""
    selected: list[CodingCommandRun] = []
    for kind in dict.fromkeys(run.kind for run in runs if include(run)):
        candidates = [run for run in runs if run.kind == kind]
        current = next(
            (
                run
                for run in candidates
                if commit_sha in {run.git_commit_before, run.git_commit_after}
            ),
            None,
        )
        selected.append(current or candidates[0])
    return tuple(selected)


def _relevant_artifacts(evidence: tuple[ProofEvidence, ...]) -> tuple[DossierArtifact, ...]:
    by_ref = {
        artifact.ref: artifact
        for item in evidence
        for artifact in item.artifacts
    }
    return tuple(by_ref.values())


def review_output_has_no_findings(output: str) -> bool:
    """Accept only the workflow protocol's exact final reviewer response."""
    non_empty_lines = [line.strip() for line in output.splitlines() if line.strip()]
    if not non_empty_lines:
        return False
    agent_markers = [index for index, line in enumerate(non_empty_lines) if line == "codex"]
    response_lines = (
        non_empty_lines[agent_markers[-1] + 1 :] if agent_markers else non_empty_lines
    )
    if not agent_markers or _CODEX_TELEMETRY_DELIMITER not in response_lines:
        return response_lines == [REVIEW_NO_FINDINGS_SENTINEL]
    delimiter_index = response_lines.index(_CODEX_TELEMETRY_DELIMITER)
    response = response_lines[:delimiter_index]
    telemetry = response_lines[delimiter_index + 1 :]
    return (
        response == [REVIEW_NO_FINDINGS_SENTINEL]
        and len(telemetry) == 2
        and _CODEX_TOKEN_COUNT.fullmatch(telemetry[0]) is not None
        and telemetry[1] == REVIEW_NO_FINDINGS_SENTINEL
    )


def _command_evidence(
    run: CodingCommandRun,
    *,
    binding: JobBinding,
    event: TimelineEvent | None,
    artifacts_by_event: dict[str, tuple[DossierArtifact, ...]],
) -> ProofEvidence:
    artifacts = artifacts_by_event.get(event.id, ()) if event is not None else ()
    return ProofEvidence(
        evidence_id=f"run:{run.id}",
        summary=f"{run.kind} {run.status} (exit {run.exit_code})",
        source="workflow",
        provenance="fact",
        commit_sha=(
            run.git_commit_after
            if run.git_commit_before == run.git_commit_after
            else None
        ),
        command_run_id=run.id,
        command=run.command,
        turn_id=None,
        assignment_id=binding.assignment.id,
        room_id=binding.room.id,
        environment_type=binding.environment_type,
        artifacts=artifacts,
    )


def _claim_evidence(
    store: CodingStore,
    event: TimelineEvent,
    *,
    binding: JobBinding,
    artifacts: tuple[DossierArtifact, ...],
) -> ProofEvidence:
    related_turn = event.related.get("turn")
    turn_id = None
    if isinstance(related_turn, str) and related_turn.isdigit():
        turn = next(
            (
                candidate
                for candidate in store.list_job_turns(event.job)
                if candidate.id == int(related_turn)
                and candidate.conversation_id == event.related.get("conversation")
            ),
            None,
        )
        turn_id = turn.id if turn is not None else None
    return ProofEvidence(
        evidence_id=f"event:{event.sequence}",
        summary=event.summary,
        source=event.source,
        provenance=event.provenance,
        commit_sha=event.related.get("commit"),
        command_run_id=None,
        command=None,
        turn_id=turn_id,
        assignment_id=event.related.get("assignment"),
        room_id=event.related.get("room"),
        environment_type=binding.environment_type,
        artifacts=artifacts,
    )


def _dossier_artifact(artifact: JobArtifact) -> DossierArtifact:
    return DossierArtifact(
        ref=artifact.ref,
        name=artifact.name,
        digest=artifact.digest,
        size=artifact.size,
        media_type=artifact.media_type,
        event_id=artifact.event_id,
        source=artifact.source,
        provenance=artifact.provenance,
    )


def _render_evidence_section(
    lines: list[str],
    heading: str,
    evidence: tuple[ProofEvidence, ...],
) -> None:
    lines.extend(["", f"## {heading}", ""])
    if not evidence:
        lines.append("None recorded.")
        return
    for item in evidence:
        label = f"{item.source} {item.provenance}"
        if item.source == "worker":
            label = "worker claim"
        commit = item.commit_sha or "unbound"
        turn = f" · turn {item.turn_id}" if item.turn_id is not None else ""
        artifacts = ", ".join(artifact.ref for artifact in item.artifacts) or "no artifact"
        lines.append(
            f"- [{label}] {item.evidence_id} · {item.summary} · commit `{commit}`"
            f"{turn} · {artifacts}"
        )


__all__ = [
    "AcceptanceItem",
    "AcceptanceResult",
    "DossierArtifact",
    "EnvironmentProvenance",
    "ProofDossier",
    "ProofEvidence",
    "acceptance_is_proven",
    "build_proof_dossier",
    "compile_acceptance_checklist",
    "render_proof_dossier",
    "review_output_has_no_findings",
]
