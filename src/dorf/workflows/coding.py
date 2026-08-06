"""Coding-to-PR policy and sequencing."""

from __future__ import annotations

import hashlib
import json
import subprocess
import time
from collections.abc import Callable
from dataclasses import dataclass
from pathlib import Path

from dorf.command_runner import CommandInterrupted, shell_command
from dorf.github_app import (
    GitHubAppConfigError,
    GitHubAppVerificationError,
    GitHubRepositoryClient,
    GitHubRepositoryError,
)
from dorf.repo_contract import RepoContract
from dorf.runtime import JobBinding
from dorf.sdk import JobExecution

from .coding_commands import CodingEnvironment, run_coding_job_command
from .coding_dossier import (
    acceptance_is_proven,
    build_proof_dossier,
    render_proof_dossier,
    review_output_has_no_findings,
)
from .coding_store import CodingCommandRun, CodingJob, CodingStore

VERIFY_GATE_FAILURE_LIMIT = 3
AFK_TERMINAL_JOB_STATUSES = frozenset({"abandoned", "merged", "rejected"})
VERIFIER_ATTENTION_KEY = "diff_verifier_attention"
DIFF_REPAIR_PREFIX = "DeepSeek diff advisory findings for the exact implementation commit"


@dataclass(frozen=True)
class WorkflowMessage:
    text: str
    error: bool = False


@dataclass(frozen=True)
class WorkflowOutcome:
    messages: tuple[WorkflowMessage, ...]
    exit_code: int = 0


class WorkflowFailure(RuntimeError):
    """A coding-workflow failure that the CLI can translate to process behavior."""

    def __init__(
        self,
        exit_code: int,
        messages: tuple[WorkflowMessage, ...],
        *,
        kind: str = "workflow",
    ) -> None:
        self.exit_code = exit_code
        self.messages = messages
        self.kind = kind
        super().__init__(messages[-1].text if messages else kind)


class PrPublicationFailed(WorkflowFailure):
    pass


class VerificationInfrastructureFailed(WorkflowFailure):
    pass


@dataclass(frozen=True)
class JobReadiness:
    failures: list[str]
    dirty_worktree: bool = False


@dataclass(frozen=True)
class ReviewThreadFeedback:
    thread_id: str
    comment_id: str
    path: str
    line: int | None
    author: str
    body: str


@dataclass(frozen=True)
class PullRequestCommentFeedback:
    comment_id: str
    author: str
    body: str
    created_at: str


@dataclass(frozen=True)
class PullRequestFeedback:
    review_threads: list[ReviewThreadFeedback]
    comments: list[PullRequestCommentFeedback]


@dataclass(frozen=True)
class AfkStart:
    action: str
    target_repo: str
    issue_number: int
    owner_token: str
    job_name: str | None
    messages: tuple[WorkflowMessage, ...]


@dataclass(frozen=True)
class AfkResume:
    job: CodingJob
    target_repo: str
    issue_number: int
    owner_token: str
    recovered_runs: int
    messages: tuple[WorkflowMessage, ...]


class AfkOwnershipLost(RuntimeError):
    pass


class GitPublicationRepairError(RuntimeError):
    pass


class CodingWorkflow:
    """Coordinate one coding Job through verification and PR publication."""

    def __init__(
        self,
        *,
        store: CodingStore,
        job: CodingJob,
        contract: RepoContract,
        github_client: Callable[[], GitHubRepositoryClient],
        execution: JobExecution | None = None,
        deepseek_diff_review: Callable[[CodingJob, str], CodingCommandRun] | None = None,
        github_app_slug: Callable[[], str] | None = None,
        sleep: Callable[[float], None] = time.sleep,
    ) -> None:
        self.store = store
        self.job = job
        self.contract = contract
        self._github_client = github_client
        self.execution = execution
        self._deepseek_diff_review = deepseek_diff_review
        self.binding = store.get_job_binding(job.job_name)
        if self.binding is None:
            raise RuntimeError(f"Job binding not found: {job.job_name}")
        self._github_app_slug = github_app_slug
        self._sleep = sleep
        self._messages: list[WorkflowMessage] = []

    def _emit(self, text: str, *, error: bool = False) -> None:
        self._messages.append(WorkflowMessage(text, error))

    def _outcome(self, exit_code: int = 0) -> WorkflowOutcome:
        return WorkflowOutcome(tuple(self._messages), exit_code)

    def _fail(
        self,
        text: str,
        *,
        exit_code: int = 1,
        kind: str = "workflow",
        failure_type: type[WorkflowFailure] = WorkflowFailure,
    ) -> None:
        self._emit(text, error=True)
        raise failure_type(exit_code, tuple(self._messages), kind=kind)

    @staticmethod
    def existing_afk_job(
        store: CodingStore,
        *,
        target_repo: str,
        issue_number: int,
    ) -> CodingJob | None:
        """Inspect the one convergent nonterminal identity without claiming it."""
        matches = [
            job
            for job in store.list_coding_jobs()
            if str(Path(job.target_repo).resolve()) == str(Path(target_repo).resolve())
            and job.metadata.get("afk_issue_number") == str(issue_number)
            and job.status not in AFK_TERMINAL_JOB_STATUSES
        ]
        if len(matches) > 1:
            message = WorkflowMessage(
                f"Multiple AFK Jobs exist for issue #{issue_number}; resume "
                "one with dorf afk-resume JOB.",
                error=True,
            )
            raise WorkflowFailure(1, (message,), kind="state")
        return matches[0] if matches else None

    @staticmethod
    def prepare_afk_start(
        store: CodingStore,
        *,
        target_repo: str,
        issue_number: int,
        owner_token: str,
    ) -> AfkStart:
        """Reserve an issue and decide whether AFK should launch or resume."""
        existing = CodingWorkflow.existing_afk_job(
            store,
            target_repo=target_repo,
            issue_number=issue_number,
        )
        try:
            reservation = store.claim_afk_coordinator(
                target_repo,
                issue_number,
                owner_token,
            )
        except RuntimeError as error:
            message = WorkflowMessage(
                f"Could not start AFK: {error}. Use afk-resume --takeover to recover.",
                error=True,
            )
            raise WorkflowFailure(1, (message,), kind="ownership") from error

        matches = [existing] if existing is not None else []
        if reservation.job_name is not None:
            job_name = reservation.job_name
            reserved_job = next((item for item in matches if item.job_name == job_name), None)
            if reserved_job is None:
                message = WorkflowMessage(
                    f"Reserved AFK Job not found: {job_name}",
                    error=True,
                )
                raise WorkflowFailure(1, (message,), kind="state")
            if reserved_job.status in {"setting-up", "setup-failed"}:
                message = WorkflowMessage(
                    "AFK Job setup was interrupted before the primary agent started: "
                    f"{job_name}. Recover it with dorf afk-resume {job_name}.",
                    error=True,
                )
                raise WorkflowFailure(1, (message,), kind="setup")
            return AfkStart(
                "resume",
                target_repo,
                issue_number,
                owner_token,
                job_name,
                (WorkflowMessage(f"Resuming AFK Job {job_name} for issue #{issue_number}"),),
            )
        if matches:
            job = matches[0]
            if job.status in {"setting-up", "setup-failed"}:
                message = WorkflowMessage(
                    "AFK Job setup was interrupted before the primary agent started: "
                    f"{job.job_name}. Recover it with dorf afk-resume "
                    f"{job.job_name}.",
                    error=True,
                )
                raise WorkflowFailure(1, (message,), kind="setup")
            store.link_afk_job(
                target_repo,
                issue_number,
                owner_token,
                job.job_name,
            )
            return AfkStart(
                "resume",
                target_repo,
                issue_number,
                owner_token,
                job.job_name,
                (WorkflowMessage(f"Resuming AFK Job {job.job_name} for issue #{issue_number}"),),
            )
        return AfkStart(
            "launch",
            target_repo,
            issue_number,
            owner_token,
            None,
            (),
        )

    @staticmethod
    def prepare_afk_resume(
        store: CodingStore,
        *,
        job_name: str,
        owner_token: str,
        takeover: bool,
        repair_attention_id: str | None = None,
        decline_attention_id: str | None = None,
    ) -> AfkResume:
        """Claim an existing AFK Job and reconcile abandoned child runs."""
        job = store.get_coding_job(job_name)
        if job is None:
            raise WorkflowFailure(
                1,
                (WorkflowMessage(f"CodingJob not found: {job_name}", True),),
                kind="state",
            )
        issue = job.metadata.get("afk_issue_number")
        if issue is None:
            raise WorkflowFailure(
                1,
                (WorkflowMessage(f"CodingJob is not AFK-coordinated: {job_name}", True),),
                kind="state",
            )
        issue_number = int(issue)
        target_repo = str(Path(job.target_repo).resolve())
        try:
            reservation = store.claim_afk_coordinator(
                target_repo,
                issue_number,
                owner_token,
                takeover=takeover,
                expected_job_name=job_name,
            )
            if reservation.job_name is None:
                store.link_afk_job(target_repo, issue_number, owner_token, job_name)
            interrupted = store.interrupt_abandoned_afk_runs(job_name) if takeover else []
        except RuntimeError as error:
            raise WorkflowFailure(
                1,
                (
                    WorkflowMessage(
                        f"Could not resume AFK: {error}",
                        True,
                    ),
                ),
                kind="ownership",
            ) from error
        decision_messages = CodingWorkflow.decide_verifier_attention(
            store,
            job_name,
            repair_attention_id=repair_attention_id,
            decline_attention_id=decline_attention_id,
        )
        messages = decision_messages + (
            (
                WorkflowMessage(
                    f"Reconciled {len(interrupted)} abandoned AFK command run(s) for {job_name}."
                ),
            )
            if interrupted
            else ()
        )
        job = store.get_coding_job(job_name) or job
        return AfkResume(
            job,
            target_repo,
            issue_number,
            owner_token,
            len(interrupted),
            messages,
        )

    def publish(self) -> WorkflowOutcome:
        """Create or update the Job PR and persist its identity."""
        if self.job.status not in {"ready", "needs-human"}:
            self._fail(f"CodingJob cannot be published: {self.job.job_name} ({self.job.status})")
        number, url, draft = self._publish_job_pr(self.job)
        self.store.record_github_pr(self.job.job_name, number, url)
        prefix = "Published draft GitHub PR" if draft else "Published GitHub PR"
        self._emit(f"{prefix} #{number}: {url}")
        return self._outcome()

    def _publish_job_pr(self, job: CodingJob) -> tuple[int, str, bool]:
        repo_full_name = job.metadata.get("github_repo")
        if not repo_full_name:
            self._fail(
                "Could not create GitHub PR: Job metadata is missing github_repo.",
                kind="publication",
            )

        draft = job.status == "needs-human" or _verifier_attention(job) is not None
        try:
            client = self._github_client()
            existing_number = job.github_pr_number
            existing_url = job.github_pr_url
            if existing_number is None:
                existing_prs = client.list_pull_requests_for_branch(
                    repo_full_name,
                    job.job_branch,
                    state="open",
                )
                if existing_prs:
                    existing = github_pr_from_payload(existing_prs[0])
                    if existing is None:
                        self._fail(
                            "Could not create GitHub PR: existing PR response did not "
                            "include number and html_url",
                            kind="publication",
                        )
                    existing_number, existing_url = existing

            if existing_number is None:
                payload = client.create_pull_request(
                    repo_full_name,
                    title=github_pr_title(job),
                    body=github_pr_body(job),
                    head=job.job_branch,
                    base=job.target_branch,
                    draft=draft,
                )
            else:
                payload = client.update_pull_request(
                    repo_full_name,
                    existing_number,
                    title=github_pr_title(job),
                    body=github_pr_body(job),
                    base=job.target_branch,
                )
                if draft:
                    client.mark_pull_request_draft(repo_full_name, existing_number)
                else:
                    client.mark_pull_request_ready(repo_full_name, existing_number)
                if existing_url and "html_url" not in payload:
                    payload["html_url"] = existing_url
            pr = github_pr_from_payload(payload)
            if pr is None:
                self._fail(
                    "Could not create GitHub PR: response did not include number and html_url",
                    kind="publication",
                )
            client.add_pull_request_comment(
                repo_full_name,
                pr[0],
                github_pr_verification_comment(self.store, job),
            )
        except WorkflowFailure:
            raise
        except GitHubAppConfigError as error:
            self._fail(
                f"github: not configured ({error})",
                kind="publication",
            )
        except (GitHubAppVerificationError, GitHubRepositoryError) as error:
            self._fail(
                f"Could not create GitHub PR: {error}",
                kind="publication",
            )
        return pr[0], pr[1], draft

    def _publish_verified(self, job: CodingJob) -> None:
        try:
            number, url, _draft = self._publish_job_pr(job)
        except WorkflowFailure as error:
            raise PrPublicationFailed(
                error.exit_code,
                error.messages,
                kind="publication",
            ) from error
        self.store.record_github_pr(job.job_name, number, url)

    def _require_execution(self) -> None:
        if self.execution is None:
            self._fail(
                "Coding workflow execution collaborators are unavailable.",
                kind="infrastructure",
            )

    def _require_environment(self) -> CodingEnvironment:
        if self.execution is None:
            self._fail(
                "Coding workflow environment is unavailable.",
                kind="infrastructure",
            )
        return self.execution

    def _run_command(self, spec):
        self._require_execution()
        try:
            return run_coding_job_command(
                store=self.store,
                environment=self.execution,
                job=self.job,
                binding=self.binding,
                contract=self.contract,
                spec=spec,
            )
        except ValueError as error:
            self._fail(str(error), kind="configuration")

    def _unsettled_turn(self):
        self._require_execution()
        for turn in reversed(self.store.list_job_turns(self.job.job_name)):
            if turn.status == "running":
                self._fail(
                    f"Job turn {turn.id} is still active for {self.job.job_name}; "
                    "wait or inspect the Job before retrying.",
                    exit_code=75,
                    kind="active-turn",
                )
            if turn.status == "recovery-required":
                return turn
        return None

    def _run_turn(self, *, kind: str, turn_key: str, prompt: str):
        self._require_execution()
        message_id = f"jmsg-{hashlib.sha256(turn_key.encode()).hexdigest()[:32]}"
        try:
            job_input, _ = self.execution.admit_message(
                message_id=message_id,
                text=prompt,
            )
            return self.execution.deliver_input(job_input.id)
        except RuntimeError as error:
            self._fail(
                f"Could not continue {kind}: {error}",
                exit_code=75 if "earlier Job input" in str(error) else 1,
                kind="active-turn" if "earlier Job input" in str(error) else "infrastructure",
            )

    def mark_ready(self) -> WorkflowOutcome:
        """Apply the mechanical readiness gate and persist a ready outcome."""
        if self.job.status in {"discarded", "running"}:
            self._fail(f"CodingJob cannot be marked ready: {self.job.job_name} ({self.job.status})")
        checklist = self.store.get_acceptance_checklist(self.job.job_name)
        if checklist is not None:
            self.store.freeze_acceptance_checklist(self.job.job_name)
        try:
            readiness = verify_job_readiness(
                self.store,
                self._require_environment(),
                self.job,
                self.contract,
                github_client=self._github_client,
            )
        except GitPublicationRepairError as error:
            self._fail(
                f"CodingJob publication repair failed for {self.job.job_name}: {error}",
                kind="publication",
            )
        if readiness.failures:
            self._emit(
                f"CodingJob is not ready: {self.job.job_name}",
                error=True,
            )
            for failure in readiness.failures:
                self._emit(f"- {failure}", error=True)
            raise WorkflowFailure(1, tuple(self._messages), kind="readiness")
        if checklist is not None:
            commit_sha = self._read_job_head()
            dossier = build_proof_dossier(
                self.store,
                self.job,
                self.binding,
                commit_sha=commit_sha,
            )
            if not acceptance_is_proven(dossier):
                self._emit(
                    f"CodingJob acceptance is unproven at {commit_sha}: {self.job.job_name}",
                    error=True,
                )
                for result in dossier.acceptance:
                    if result.status != "proven":
                        self._emit(f"- {result.reason}", error=True)
                raise WorkflowFailure(1, tuple(self._messages), kind="readiness")
            self.store.set_metadata_value(self.job.job_name, "proof_commit", commit_sha)
        self.store.update_status(self.job.job_name, "ready")
        self._emit(f"CodingJob ready: {self.job.job_name}")
        return self._outcome()

    def _check_gate(self) -> dict | None:
        for name in ("check", "smoke"):
            command = self.contract.commands.get(name)
            if command is None:
                continue
            run = self._run_command(shell_command(name, command))
            if run.exit_code == 0:
                continue
            return {
                "type": name,
                "job_name": self.job.job_name,
                "job_branch": self.job.job_branch,
                "target_start_sha": self.job.target_start_sha,
                "failure": {
                    "kind": name,
                    "run_id": run.id,
                    "exit_code": run.exit_code,
                    "output_path": run.output_path,
                    "message": (
                        f"{name} did not finish"
                        if run.exit_code is None
                        else f"{name} exited with code {run.exit_code}"
                    ),
                },
            }
        return None

    def _ready_gate(self) -> dict | None:
        readiness = verify_job_readiness(
            self.store,
            self._require_environment(),
            self.job,
            self.contract,
            github_client=self._github_client,
        )
        if not readiness.failures:
            return None
        return {
            "type": "ready",
            "job_name": self.job.job_name,
            "job_branch": self.job.job_branch,
            "target_start_sha": self.job.target_start_sha,
            "failures": readiness.failures,
            "failure_codes": (["dirty_worktree"] if readiness.dirty_worktree else []),
        }

    def _mark_needs_human_and_publish(self) -> None:
        self.store.update_status(self.job.job_name, "needs-human")
        job = self.store.get_coding_job(self.job.job_name)
        if job is None:
            self._fail(f"CodingJob not found: {self.job.job_name}")
        self.job = job
        self._publish_verified(job)

    def _run_verify_fix(self, payload: dict) -> None:
        run = self._run_turn(
            kind="verify:fix",
            turn_key=verify_fix_turn_key(payload),
            prompt=verify_fix_prompt(payload),
        )
        if run.exit_code != 0:
            self._mark_needs_human_and_publish()
            self._fail(
                f"verify:fix failed for {self.job.job_name} with exit code {run.exit_code}.",
                exit_code=run.exit_code or 1,
                kind="needs-human",
            )
        self._emit(f"verify:fix succeeded for {self.job.job_name}")

    @staticmethod
    def decide_verifier_attention(
        store: CodingStore,
        job_name: str,
        *,
        repair_attention_id: str | None = None,
        decline_attention_id: str | None = None,
    ) -> tuple[WorkflowMessage, ...]:
        if repair_attention_id and decline_attention_id:
            raise WorkflowFailure(
                2,
                (WorkflowMessage("Choose repair or decline, not both.", True),),
                kind="usage",
            )
        decision_id = repair_attention_id or decline_attention_id
        if decision_id is None:
            return ()
        job = store.get_coding_job(job_name)
        attention = _verifier_attention(job) if job is not None else None
        if (
            attention is None
            or attention.get("id") != decision_id
            or attention.get("status") != "outstanding"
        ):
            raise WorkflowFailure(
                1,
                (WorkflowMessage(f"Attention item cannot be decided: {decision_id}", True),),
                kind="verifier-attention",
            )
        status = "approved" if repair_attention_id else "declined"
        attention["status"] = status
        if status == "approved":
            attention["after_run_id"] = max(
                (run.id for run in store.list_command_runs(job_name)), default=0
            )
        store.set_metadata_value(job_name, VERIFIER_ATTENTION_KEY, json.dumps(attention))
        return (WorkflowMessage(f"Attention {decision_id}: {status}."),)

    def _raise_verifier_attention(self) -> None:
        attention = _verifier_attention(self.job)
        if attention is None or attention.get("status") in {"approved", "retrying"}:
            return
        if attention.get("status") == "declined":
            self._mark_needs_human_and_publish()
            self._fail(
                "DeepSeek diff advisory review was declined; no review evidence was "
                "manufactured and the PR remains draft.",
                kind="needs-human",
            )
        self._publish_verified(self.job)
        attention_id = str(attention["id"])
        command = "afk-resume" if self.job.metadata.get("afk_issue_number") else "verify"
        self._fail(
            f"DeepSeek verifier repair required. Run dorf {command} {self.job.job_name} "
            f"--repair-attention {attention_id}, or use --decline-attention {attention_id}.",
            exit_code=75,
            kind="verifier-attention",
        )

    def _record_verifier_attention(
        self,
        commit: str,
        run: CodingCommandRun,
    ) -> None:
        attention_id = "attention-" + hashlib.sha256(
            f"{self.job.job_name}:{commit}:{run.id}".encode()
        ).hexdigest()[:24]
        self.store.set_metadata_values(
            self.job.job_name,
            {
                VERIFIER_ATTENTION_KEY: json.dumps(
                    {
                        "id": attention_id,
                        "status": "outstanding",
                        "commit": commit,
                        "failed_run_id": run.id,
                    },
                    sort_keys=True,
                ),
                "afk_stage": "blocked",
                "afk_outcome": "DeepSeek verifier requires repair or decline",
            },
        )
        current = self.store.get_coding_job(self.job.job_name)
        if current is not None:
            self.job = current
        self._raise_verifier_attention()

    def _review_exact_commit(self, commit: str) -> CodingCommandRun:
        attention = _verifier_attention(self.job)
        retrying = attention is not None and attention.get("status") in {
            "approved",
            "retrying",
        }
        after_run_id = int(attention.get("after_run_id", 0)) if retrying else 0
        run = next(
            (
                candidate
                for candidate in self.store.list_command_runs(self.job.job_name)
                if candidate.kind == "verify-role:diff"
                and candidate.git_commit_before == commit
                and candidate.id > after_run_id
                and candidate.status != "interrupted"
            ),
            None,
        )
        if run is not None and run.status == "running":
            self._fail(
                f"DeepSeek verifier run {run.id} is still active.",
                exit_code=75,
                kind="active-command",
            )
        if run is None:
            if self._deepseek_diff_review is None:
                run = self.store.record_command_run(
                    self.job.job_name,
                    "verify-role:diff",
                    f"pi deepseek-v4-flash diff at {commit}",
                    "failed",
                    1,
                    git_commit_before=commit,
                )
                self._record_verifier_attention(commit, run)
            if retrying and attention is not None and attention.get("status") == "approved":
                attention["status"] = "retrying"
                self.store.set_metadata_value(
                    self.job.job_name, VERIFIER_ATTENTION_KEY, json.dumps(attention)
                )
            try:
                run = self._deepseek_diff_review(self.job, commit)
            except CommandInterrupted:
                raise
            except RuntimeError:
                run = self.store.record_command_run(
                    self.job.job_name,
                    "verify-role:diff",
                    f"pi deepseek-v4-flash diff at {commit}",
                    "failed",
                    1,
                    git_commit_before=commit,
                )
        if run.exit_code != 0 or run.status != "succeeded":
            self._record_verifier_attention(commit, run)
        if run.git_commit_before != commit or run.git_commit_after != commit:
            self._fail("DeepSeek verifier result is not pinned to the implementation commit.")
        if retrying:
            self.store.remove_metadata_keys(
                self.job.job_name,
                {VERIFIER_ATTENTION_KEY, "afk_stage", "afk_outcome"},
            )
            refreshed = self.store.get_coding_job(self.job.job_name)
            if refreshed is not None:
                self.job = refreshed
        return run

    def _finish_verified(self, commit_sha: str) -> WorkflowOutcome:
        checklist = self.store.get_acceptance_checklist(self.job.job_name)
        if checklist is not None:
            dossier = build_proof_dossier(
                self.store,
                self.job,
                self.binding,
                commit_sha=commit_sha,
            )
            self.store.set_metadata_value(self.job.job_name, "proof_commit", commit_sha)
            if not acceptance_is_proven(dossier):
                self._mark_needs_human_and_publish()
                self._fail(
                    f"Verify stopped for {self.job.job_name}: acceptance remains "
                    f"unproven at {commit_sha}.",
                    kind="needs-human",
                )
        self.store.update_status(self.job.job_name, "ready")
        job = self.store.get_coding_job(self.job.job_name)
        if job is None:
            self._fail(f"CodingJob not found: {self.job.job_name}")
        self.job = job
        self._publish_verified(job)
        self._emit(f"Verify passed for {self.job.job_name}")
        return self._outcome()

    def _review_verdict(self, run: CodingCommandRun) -> tuple[bool, str]:
        event_id = f"evt-deepseek-{run.id}-verdict"
        events = self.store.documents.list_events(self.job.job_name)
        existing = next(
            (event for event in events if event.id == event_id),
            None,
        )
        output = command_run_output(run.output_path)
        if existing is not None:
            return existing.related.get("verdict") == "no-findings", output
        no_findings = review_output_has_no_findings(output)
        verdict = "no-findings" if no_findings else "findings"
        self.store.documents.append_event(
            self.job.job_name,
            event_id=event_id,
            source="workflow",
            provenance="fact",
            kind="review-verdict",
            summary=f"DeepSeek diff review observed {verdict}",
            related={
                "commit": run.git_commit_after or "",
                "run": str(run.id),
                "verdict": verdict,
            },
        )
        return no_findings, output

    def verify(
        self,
        *,
        continue_guard: Callable[[], None] | None = None,
        repair_attention_id: str | None = None,
        decline_attention_id: str | None = None,
    ) -> WorkflowOutcome:
        """Run checks and one bounded DeepSeek advisory repair before publication."""
        self._messages.extend(
            self.decide_verifier_attention(
                self.store,
                self.job.job_name,
                repair_attention_id=repair_attention_id,
                decline_attention_id=decline_attention_id,
            )
        )
        current = self.store.get_coding_job(self.job.job_name)
        if current is not None:
            self.job = current
        self._raise_verifier_attention()
        if self.store.get_acceptance_checklist(self.job.job_name) is not None:
            self.store.freeze_acceptance_checklist(self.job.job_name)

        recovered = self._unsettled_turn()
        if recovered is not None and recovered.exit_code != 0:
            self._mark_needs_human_and_publish()
            self._fail(
                f"Verify found unrecovered Job turn {recovered.id}: {recovered.status}.",
                exit_code=recovered.exit_code or 1,
                kind="needs-human",
            )

        gate_failures = 0
        try:
            while True:
                if continue_guard is not None:
                    continue_guard()
                payload = self._check_gate()
                if payload is None:
                    try:
                        payload = self._ready_gate()
                    except GitPublicationRepairError as error:
                        self._fail(
                            f"Verify infrastructure failed for {self.job.job_name}: {error}",
                            kind="infrastructure",
                            failure_type=VerificationInfrastructureFailed,
                        )
                if payload is not None:
                    if continue_guard is not None:
                        continue_guard()
                    gate_failures += 1
                    if gate_failures > VERIFY_GATE_FAILURE_LIMIT:
                        self._mark_needs_human_and_publish()
                        self._fail(
                            f"Verify stopped for {self.job.job_name}: gate failed "
                            f"{gate_failures} time(s).",
                            kind="needs-human",
                        )
                    self._run_verify_fix(payload)
                    continue
                commit = self._read_job_head()
                run = self._review_exact_commit(commit)
                if continue_guard is not None:
                    continue_guard()
                no_findings, output = self._review_verdict(run)
                if no_findings:
                    return self._finish_verified(commit)
                if any(
                    DIFF_REPAIR_PREFIX in item.text
                    for item in self.store.list_job_inputs(self.job.job_name)
                ):
                    self._mark_needs_human_and_publish()
                    self._fail(
                        "DeepSeek reported findings after the one bounded advisory repair.",
                        kind="needs-human",
                    )
                payload = {
                    "type": "deepseek-diff-findings",
                    "job_name": self.job.job_name,
                    "commit": commit,
                    "run_id": run.id,
                    "findings": output or "Verifier returned no structured response.",
                }
                self._run_verify_fix(payload)
                if self._read_job_head() == commit:
                    self._mark_needs_human_and_publish()
                    self._fail(
                        "The implementation Worker did not commit its findings decision.",
                        kind="needs-human",
                    )
        except CommandInterrupted:
            self._fail(
                f"Verify interrupted for {self.job.job_name}.",
                exit_code=130,
                kind="interrupted",
            )

    def _ensure_followup_at_pr_head(self, pr: dict) -> bool:
        pr_head = github_pr_head_sha_or_exit(pr)
        environment = self._require_environment()
        status = git_output(environment, self.binding, "status", "--porcelain")
        if status.returncode != 0:
            raise RuntimeError(
                f"could not read Job working tree status: {git_command_message(status)}"
            )
        if status.stdout.strip():
            raise RuntimeError("Job working tree is dirty; cannot sync PR feedback safely")

        head = git_output(environment, self.binding, "rev-parse", "HEAD")
        if head.returncode != 0:
            raise RuntimeError(f"could not read Job HEAD: {git_command_message(head)}")
        if head.stdout.strip() == pr_head:
            return False
        if job_head_is_ahead(
            environment,
            self.binding,
            remote_head=pr_head,
        ):
            return True

        fetch = git_output(
            environment,
            self.binding,
            "fetch",
            "origin",
            self.job.job_branch,
        )
        if fetch.returncode != 0 and git_auth_failed(fetch):
            try:
                environment.refresh_git_credentials()
            except RuntimeError as error:
                raise GitPublicationRepairError(str(error)) from error
            fetch = git_output(
                environment,
                self.binding,
                "fetch",
                "origin",
                self.job.job_branch,
            )
        if fetch.returncode != 0:
            raise RuntimeError(
                f"could not fetch PR branch in Job workspace: {git_command_message(fetch)}"
            )

        remote_ahead = git_output(
            environment,
            self.binding,
            "merge-base",
            "--is-ancestor",
            "HEAD",
            "FETCH_HEAD",
        )
        if remote_ahead.returncode == 0:
            merge = git_output(
                environment,
                self.binding,
                "merge",
                "--ff-only",
                "FETCH_HEAD",
            )
            if merge.returncode != 0:
                raise RuntimeError(
                    f"could not fast-forward Job branch to PR head: {git_command_message(merge)}"
                )
            updated = git_output(environment, self.binding, "rev-parse", "HEAD")
            if updated.returncode != 0:
                raise RuntimeError(
                    f"could not read updated Job HEAD: {git_command_message(updated)}"
                )
            if updated.stdout.strip() != pr_head:
                raise RuntimeError("Job branch did not fast-forward to the linked PR head")
            return False
        if remote_ahead.returncode != 1:
            raise GitPublicationRepairError(
                "could not compare Job HEAD with the fetched PR head: "
                f"{git_command_message(remote_ahead)}"
            )
        local_ahead = git_output(
            environment,
            self.binding,
            "merge-base",
            "--is-ancestor",
            "FETCH_HEAD",
            "HEAD",
        )
        if local_ahead.returncode == 0:
            return True
        if local_ahead.returncode != 1:
            raise GitPublicationRepairError(
                "could not compare the fetched PR head with Job HEAD: "
                f"{git_command_message(local_ahead)}"
            )
        raise RuntimeError("Job branch has diverged from the linked PR head")

    def _collect_feedback(
        self,
        client: GitHubRepositoryClient,
        repo_full_name: str,
    ) -> PullRequestFeedback:
        if self._github_app_slug is None:
            raise RuntimeError("GitHub App metadata is unavailable")
        app_slug = self._github_app_slug()
        assert self.job.github_pr_number is not None
        threads = fetch_unresolved_review_threads(
            client,
            repo_full_name,
            self.job.github_pr_number,
            dorf_app_slug=app_slug,
            warn=lambda text: self._emit(text, error=True),
        )
        comments = fetch_top_level_pr_comments(
            client,
            repo_full_name,
            self.job.github_pr_number,
            dorf_app_slug=app_slug,
        )
        return PullRequestFeedback(
            review_threads=[
                item
                for item in threads
                if self.store.get_followup_feedback(
                    self.job.job_name,
                    "review-thread",
                    review_thread_feedback_ref(item),
                )
                is None
            ],
            comments=[
                item
                for item in comments
                if self.store.get_followup_feedback(
                    self.job.job_name,
                    "pr-comment",
                    item.comment_id,
                )
                is None
            ],
        )

    def _read_job_head(self) -> str:
        result = git_output(
            self._require_environment(),
            self.binding,
            "rev-parse",
            "HEAD",
        )
        if result.returncode != 0:
            self._fail(
                f"Could not read Job HEAD: {git_command_message(result)}",
                exit_code=result.returncode or 1,
                kind="infrastructure",
            )
        return result.stdout.strip()

    def _record_feedback(
        self,
        feedback: PullRequestFeedback,
        *,
        status: str,
        commit_sha: str,
    ) -> None:
        try:
            for thread in feedback.review_threads:
                self.store.record_followup_feedback(
                    self.job.job_name,
                    kind="review-thread",
                    ref_id=review_thread_feedback_ref(thread),
                    comment_id=thread.comment_id,
                    status=status,
                    commit_sha=commit_sha,
                )
            for comment in feedback.comments:
                self.store.record_followup_feedback(
                    self.job.job_name,
                    kind="pr-comment",
                    ref_id=comment.comment_id,
                    comment_id=comment.comment_id,
                    status=status,
                    commit_sha=commit_sha,
                )
        except RuntimeError as error:
            self._fail(
                f"Could not record PR feedback: {error}",
                kind="persistence",
            )

    def _reply_to_feedback(
        self,
        client: GitHubRepositoryClient,
        repo_full_name: str,
        feedback: PullRequestFeedback,
        commit_sha: str,
    ) -> None:
        assert self.job.github_pr_number is not None
        try:
            for thread in feedback.review_threads:
                client.add_pull_request_review_reply(
                    repo_full_name,
                    self.job.github_pr_number,
                    int(thread.comment_id),
                    followup_reply_body(commit_sha),
                )
                self.store.record_followup_feedback(
                    self.job.job_name,
                    kind="review-thread",
                    ref_id=review_thread_feedback_ref(thread),
                    comment_id=thread.comment_id,
                    status="replied",
                    commit_sha=commit_sha,
                )
            for comment in feedback.comments:
                client.add_pull_request_comment(
                    repo_full_name,
                    self.job.github_pr_number,
                    followup_top_level_reply_body(comment, commit_sha),
                )
                self.store.record_followup_feedback(
                    self.job.job_name,
                    kind="pr-comment",
                    ref_id=comment.comment_id,
                    comment_id=comment.comment_id,
                    status="replied",
                    commit_sha=commit_sha,
                )
        except RuntimeError as error:
            self._fail(
                f"Could not reply to PR feedback: {error}",
                kind="github",
            )

    def followup(self) -> WorkflowOutcome:
        """Collect linked PR feedback, repair it, verify it, and reply."""
        if self.job.github_pr_number is None:
            self._fail(f"CodingJob has no linked GitHub PR: {self.job.job_name}")
        repo_full_name = self.job.metadata.get("github_repo")
        if not repo_full_name:
            self._fail("Could not fetch PR feedback: Job metadata is missing github_repo.")
        if self.job.status not in {"ready", "needs-human"}:
            self._fail(
                f"CodingJob cannot process followup: {self.job.job_name} ({self.job.status})"
            )

        recovered = self._unsettled_turn()
        if recovered is not None and recovered.exit_code != 0:
            self.store.update_status(self.job.job_name, "needs-human")
            self._fail(
                f"Followup found unrecovered Job turn {recovered.id}: {recovered.status}.",
                exit_code=recovered.exit_code or 1,
                kind="needs-human",
            )

        try:
            client = self._github_client()
            pr = client.get_pull_request(repo_full_name, self.job.github_pr_number)
            if pr.get("state") != "open":
                self._fail(f"Linked GitHub PR is not open: #{self.job.github_pr_number}")
            if self._ensure_followup_at_pr_head(pr):
                self.verify()
                pr = client.get_pull_request(repo_full_name, self.job.github_pr_number)
                if pr.get("state") != "open":
                    raise RuntimeError(
                        f"linked GitHub PR is no longer open: #{self.job.github_pr_number}"
                    )
                if self._ensure_followup_at_pr_head(pr):
                    raise GitPublicationRepairError(
                        "verification completed but Job HEAD remains ahead of the PR branch"
                    )
            feedback = self._collect_feedback(client, repo_full_name)
        except WorkflowFailure:
            raise
        except GitPublicationRepairError as error:
            self._fail(
                f"Could not repair Job branch publication: {error}",
                kind="publication",
            )
        except RuntimeError as error:
            self._fail(
                f"Could not fetch PR feedback: {error}",
                kind="github",
            )

        if not pull_request_feedback_needs_fix(feedback):
            self._emit(f"No new PR feedback for {self.job.job_name}.")
            return self._outcome()

        prompt = followup_fix_prompt(
            job=self.job,
            pr_number=self.job.github_pr_number,
            feedback=feedback,
        )
        try:
            run = self._run_turn(
                kind="followup:fix",
                turn_key=followup_turn_key(self.job.github_pr_number, prompt),
                prompt=prompt,
            )
        except CommandInterrupted:
            self._fail(
                f"Followup interrupted for {self.job.job_name}.",
                exit_code=130,
                kind="interrupted",
            )
        if run.exit_code != 0:
            self.store.update_status(self.job.job_name, "needs-human")
            self._fail(
                f"followup:fix failed for {self.job.job_name} with exit code {run.exit_code}.",
                exit_code=run.exit_code or 1,
                kind="needs-human",
            )

        self.verify()
        updated = self.store.get_coding_job(self.job.job_name)
        if updated is None:
            self._fail(f"CodingJob not found: {self.job.job_name}")
        self.job = updated
        head = self._read_job_head()
        self._record_feedback(feedback, status="verified", commit_sha=head)
        self._reply_to_feedback(client, repo_full_name, feedback, head)
        self._emit(f"Followup passed for {self.job.job_name} at {head}")
        return self._outcome()

    def _set_afk_progress(self, stage: str, outcome: str) -> None:
        self.store.set_metadata_values(
            self.job.job_name,
            {"afk_stage": stage, "afk_outcome": outcome},
        )

    def _implementation_turn(self):
        inputs = self.store.list_job_inputs(self.job.job_name)
        if not inputs or inputs[0].kind != "goal":
            self._fail(f"Implementation input not found: {self.job.job_name}")
        return self.store.get_job_turn_by_input(self.job.job_name, inputs[0].id)

    def _wait_for_implementation(
        self,
        continue_guard: Callable[[], None],
    ):
        while True:
            continue_guard()
            implementation = self._implementation_turn()
            current = self.store.get_coding_job(self.job.job_name)
            if current is None:
                self._fail(f"Coding Job not found: {self.job.job_name}")
            self.job = current
            if implementation is not None and (
                current.status in AFK_TERMINAL_JOB_STATUSES or implementation.status != "running"
            ):
                return implementation
            self._sleep(0.25)

    def coordinate_afk(
        self,
        *,
        issue_number: int,
        target_repo: str,
        owner_token: str,
    ) -> WorkflowOutcome:
        """Resume AFK work from persisted state through its terminal outcome."""

        def ownership_guard() -> None:
            try:
                self.store.assert_afk_coordinator_owner(target_repo, issue_number, owner_token)
            except RuntimeError as error:
                raise AfkOwnershipLost(str(error)) from error

        try:
            ownership_guard()
        except AfkOwnershipLost:
            self._fail(
                f"AFK ownership was replaced for {self.job.job_name}; stopping.",
                kind="ownership",
            )

        if self.job.status in AFK_TERMINAL_JOB_STATUSES:
            self.store.finish_afk_coordinator(
                target_repo,
                issue_number,
                owner_token,
                self.job.status,
            )
            self._fail(f"AFK cannot coordinate terminal Job {self.job.job_name}: {self.job.status}")

        stage = self.job.metadata.get("afk_stage")
        if self.job.status in {"ready", "needs-human"} and self.job.github_pr_number is not None:
            outcome = self.job.status
            self._set_afk_progress(outcome, f"{outcome} PR published")
            running = [
                run
                for run in self.store.list_command_runs(self.job.job_name)
                if run.kind == "afk" and run.status == "running"
            ]
            implementation = self._implementation_turn()
            failure_code = implementation.exit_code or 1 if implementation else 1
            if running:
                self.store.finish_command_run(
                    running[-1].id,
                    "succeeded" if outcome == "ready" else "failed",
                    0 if outcome == "ready" else failure_code,
                )
            self.store.finish_afk_coordinator(target_repo, issue_number, owner_token, outcome)
            self._emit(f"AFK already finished for {self.job.job_name}: {outcome}")
            if outcome == "needs-human":
                raise WorkflowFailure(1, tuple(self._messages), kind="needs-human")
            return self._outcome()

        afk_runs = [
            run for run in self.store.list_command_runs(self.job.job_name) if run.kind == "afk"
        ]
        afk_run = next(
            (run for run in afk_runs if run.status == "running"),
            None,
        )
        if afk_run is None:
            afk_run = self.store.create_command_run(
                self.job.job_name,
                "afk",
                f"dorf afk {issue_number}",
                "",
            )

        if self.job.status in {"ready", "needs-human"} or stage in {"ready", "needs-human"}:
            outcome = self.job.status if self.job.status in {"ready", "needs-human"} else stage
            if self.job.status != outcome:
                self.store.update_status(self.job.job_name, outcome)
                current = self.store.get_coding_job(self.job.job_name)
                if current is None:
                    self._fail(f"CodingJob not found: {self.job.job_name}")
                self.job = current
            self._set_afk_progress("publishing", f"retrying {outcome} PR publication")
            self._publish_verified(self.job)
            self._set_afk_progress(outcome, f"{outcome} PR published")
            implementation = self._implementation_turn()
            failure_code = implementation.exit_code or 1 if implementation else 1
            self.store.finish_command_run(
                afk_run.id,
                "succeeded" if outcome == "ready" else "failed",
                0 if outcome == "ready" else failure_code,
            )
            self.store.finish_afk_coordinator(target_repo, issue_number, owner_token, outcome)
            if outcome == "needs-human":
                raise WorkflowFailure(1, tuple(self._messages), kind="needs-human")
            self._emit(f"AFK complete for {self.job.job_name}")
            return self._outcome()

        self._set_afk_progress("implementation", "waiting for primary agent")
        try:
            implementation = self._wait_for_implementation(ownership_guard)
            ownership_guard()
        except AfkOwnershipLost:
            self._fail(
                f"AFK ownership was replaced for {self.job.job_name}; stopping.",
                kind="ownership",
            )

        current = self.store.get_coding_job(self.job.job_name)
        if current is None:
            self._fail(f"CodingJob not found: {self.job.job_name}")
        self.job = current
        if current.status in AFK_TERMINAL_JOB_STATUSES:
            self._set_afk_progress(current.status, "coordination stopped by lifecycle")
            self.store.finish_command_run(afk_run.id, "interrupted", 130)
            self.store.finish_afk_coordinator(
                target_repo, issue_number, owner_token, current.status
            )
            self._fail(f"AFK stopped for terminal Job {self.job.job_name}: {current.status}")

        if implementation.status != "succeeded" or implementation.exit_code != 0:
            exit_code = implementation.exit_code or 1
            outcome = f"implementation {implementation.status} (exit {exit_code})"
            self.store.update_status(self.job.job_name, "needs-human")
            self._set_afk_progress("publishing", f"{outcome}; PR publication pending")
            failed = self.store.get_coding_job(self.job.job_name)
            if failed is None:
                self._fail(f"CodingJob not found: {self.job.job_name}")
            self.job = failed
            self._publish_verified(failed)
            self._set_afk_progress("needs-human", outcome)
            self.store.finish_command_run(afk_run.id, "failed", exit_code)
            self.store.finish_afk_coordinator(target_repo, issue_number, owner_token, "needs-human")
            self._fail(
                f"Implementation failed for {self.job.job_name}: exit {exit_code}",
                exit_code=exit_code,
                kind="needs-human",
            )

        self._set_afk_progress("verifying", "implementation succeeded")
        self._emit(f"Implementation succeeded for {self.job.job_name}; starting verify")
        try:
            self.verify(continue_guard=ownership_guard)
        except PrPublicationFailed:
            current = self.store.get_coding_job(self.job.job_name)
            if current is not None:
                self.job = current
                self._set_afk_progress(
                    "publishing",
                    f"{current.status} outcome; PR publication pending",
                )
            raise
        except VerificationInfrastructureFailed:
            self._set_afk_progress(
                "verifying",
                "verification infrastructure failed; retry with afk-resume",
            )
            raise
        except WorkflowFailure as error:
            current = self.store.get_coding_job(self.job.job_name)
            if error.kind == "verifier-attention":
                if current is not None:
                    self.job = current
                attention = _verifier_attention(current)
                self.store.finish_command_run(afk_run.id, "failed", error.exit_code or 1)
                self.store.finish_afk_coordinator(
                    target_repo,
                    issue_number,
                    owner_token,
                    str(attention.get("status", "outstanding"))
                    if attention is not None
                    else "outstanding",
                )
                raise
            if (
                current is not None
                and current.status in {"ready", "needs-human"}
                and current.github_pr_number is None
            ):
                self.job = current
                self._set_afk_progress(
                    "publishing",
                    f"{current.status} outcome; PR publication pending",
                )
                raise
            self._set_afk_progress("needs-human", "verify requires human")
            self.store.finish_command_run(afk_run.id, "failed", error.exit_code or 1)
            self.store.finish_afk_coordinator(target_repo, issue_number, owner_token, "needs-human")
            if current is not None and current.status != "needs-human":
                self.store.update_status(self.job.job_name, "needs-human")
                current = self.store.get_coding_job(self.job.job_name)
                if current is not None:
                    self.job = current
                    self._publish_verified(current)
            raise

        self._set_afk_progress("ready", "verify passed and PR published")
        self.store.finish_command_run(afk_run.id, "succeeded", 0)
        self.store.finish_afk_coordinator(target_repo, issue_number, owner_token, "ready")
        self._emit(f"AFK complete for {self.job.job_name}")
        return self._outcome()


def _verifier_attention(job: CodingJob | None) -> dict | None:
    if job is None or (raw := job.metadata.get(VERIFIER_ATTENTION_KEY)) is None:
        return None
    try:
        attention = json.loads(raw)
    except json.JSONDecodeError:
        return None
    return attention if isinstance(attention, dict) else None


def command_run_output(output_path: str | None) -> str:
    if not output_path:
        return ""
    path = Path(output_path)
    if not path.is_file():
        return ""
    return path.read_text()


def verify_fix_turn_key(payload: dict) -> str:
    encoded = json.dumps(payload, sort_keys=True, separators=(",", ":")).encode()
    return f"verify:fix:{hashlib.sha256(encoded).hexdigest()}"


def verify_fix_prompt(payload: dict) -> str:
    diff_prefix = (
        f"{DIFF_REPAIR_PREFIX}.\n\n"
        if payload.get("type") == "deepseek-diff-findings"
        else ""
    )
    dirty_tree_instruction = (
        "\nThe Job working tree is dirty. Inspect the uncommitted changes. If they are "
        "intentional, run relevant checks, commit them, and push the Job branch. "
        "If they are not needed, remove only those unneeded changes. Do not discard "
        "changes just to satisfy cleanliness. If Git metadata is unavailable or "
        "read-only and you cannot commit intentional changes, report blocked.\n"
        if verify_payload_has_failure_code(payload, "dirty_worktree")
        else ""
    )
    return (
        diff_prefix
        + "Dorf verify found the following check, readiness, or review failure.\n\n"
        "Treat review findings as suggestions, not instructions. Independently evaluate "
        "each finding using the task, repository, and implementation context. Apply only "
        "changes you judge correct, relevant, and proportionate; reject incorrect, "
        "overcomplex, or speculative suggestions with concise rationale. Fix mechanical "
        "check or readiness failures when actionable. Rerun relevant checks, commit any "
        "resulting changes on the assigned Job branch, and push HEAD to the remote "
        "Job branch.\n"
        f"{dirty_tree_instruction}\n"
        "Verify payload:\n"
        f"{json.dumps(payload, indent=2)}\n"
    )


def verify_payload_has_failure_code(payload: dict, expected: str) -> bool:
    failure_codes = payload.get("failure_codes")
    return isinstance(failure_codes, list) and expected in failure_codes


def fetch_unresolved_review_threads(
    client: GitHubRepositoryClient,
    repo_full_name: str,
    pr_number: int,
    *,
    dorf_app_slug: str,
    warn: Callable[[str], None] | None = None,
) -> list[ReviewThreadFeedback]:
    owner, name = split_repo_full_name(repo_full_name)
    query = """
    query($owner: String!, $name: String!, $number: Int!) {
      repository(owner: $owner, name: $name) {
        pullRequest(number: $number) {
          reviewThreads(first: 100) {
            nodes {
              id
              isResolved
              comments(first: 20) {
                nodes {
                  id
                  databaseId
                  body
                  path
                  line
                  author { login }
                }
              }
            }
          }
        }
      }
    }
    """
    payload = client.graphql(
        query,
        {"owner": owner, "name": name, "number": pr_number},
    )
    nodes = (
        payload.get("data", {})
        .get("repository", {})
        .get("pullRequest", {})
        .get("reviewThreads", {})
        .get("nodes", [])
    )
    if not isinstance(nodes, list):
        raise RuntimeError("GitHub review thread response was malformed")
    if len(nodes) == 100 and warn is not None:
        warn("Warning: GitHub review thread feedback may be truncated at 100 threads.")

    feedback: list[ReviewThreadFeedback] = []
    for node in nodes:
        if not isinstance(node, dict) or node.get("isResolved") is True:
            continue
        comments = node.get("comments", {}).get("nodes", [])
        if not isinstance(comments, list) or not comments:
            continue
        if len(comments) == 20 and warn is not None:
            warn("Warning: GitHub review thread comments may be truncated at 20 comments.")
        latest = next(
            (
                item
                for item in reversed(comments)
                if isinstance(item, dict) and not is_dorf_graphql_comment(item, dorf_app_slug)
            ),
            None,
        )
        if latest is None:
            continue
        database_id = latest.get("databaseId")
        body = latest.get("body")
        path = latest.get("path")
        if (
            not isinstance(database_id, int)
            or not isinstance(body, str)
            or not isinstance(path, str)
        ):
            continue
        line = latest.get("line")
        author = latest.get("author")
        feedback.append(
            ReviewThreadFeedback(
                thread_id=str(node.get("id") or database_id),
                comment_id=str(database_id),
                path=path,
                line=line if isinstance(line, int) else None,
                author=author.get("login", "unknown") if isinstance(author, dict) else "unknown",
                body=body,
            )
        )
    return feedback


def fetch_top_level_pr_comments(
    client: GitHubRepositoryClient,
    repo_full_name: str,
    pr_number: int,
    *,
    dorf_app_slug: str,
) -> list[PullRequestCommentFeedback]:
    payload = client.list_pull_request_comments(repo_full_name, pr_number)
    if not isinstance(payload, list):
        raise RuntimeError("GitHub PR comment response was malformed")
    comments: list[PullRequestCommentFeedback] = []
    for item in payload:
        if not isinstance(item, dict):
            continue
        if is_dorf_rest_comment(item, dorf_app_slug):
            continue
        user = item.get("user")
        comment_id = item.get("id")
        body = item.get("body")
        created_at = item.get("created_at")
        if not isinstance(comment_id, int) or not isinstance(body, str):
            continue
        comments.append(
            PullRequestCommentFeedback(
                comment_id=str(comment_id),
                author=user.get("login", "unknown") if isinstance(user, dict) else "unknown",
                body=body,
                created_at=created_at if isinstance(created_at, str) else "",
            )
        )
    return comments


def followup_fix_prompt(
    *,
    job: CodingJob,
    pr_number: int,
    feedback: PullRequestFeedback,
) -> str:
    payload = {
        "type": "pr-followup",
        "job_name": job.job_name,
        "pr_number": pr_number,
        "job_branch": job.job_branch,
        "review_threads": [
            {
                "thread_id": item.thread_id,
                "comment_id": item.comment_id,
                "path": item.path,
                "line": item.line,
                "author": item.author,
                "body": item.body,
            }
            for item in feedback.review_threads
        ],
        "top_level_comments": [
            {
                "comment_id": item.comment_id,
                "author": item.author,
                "created_at": item.created_at,
                "body": item.body,
            }
            for item in feedback.comments
        ],
    }
    return (
        "GitHub PR feedback was posted for this Dorf Job.\n\n"
        "Fix actionable feedback under the project principles. If a comment asks for "
        "unjustified complexity or is not actionable, leave concise rationale in your "
        "final response. Run relevant checks, commit the result on the assigned Job "
        "branch, and push HEAD to the remote Job branch.\n\n"
        "Followup payload:\n"
        f"{json.dumps(payload, indent=2)}\n"
    )


def followup_turn_key(pr_number: int, prompt: str) -> str:
    return f"followup:pr:{pr_number}:{hashlib.sha256(prompt.encode()).hexdigest()}"


def followup_reply_body(commit_sha: str) -> str:
    return f"Dorf followup addressed this feedback in {commit_sha}."


def pull_request_feedback_needs_fix(feedback: PullRequestFeedback) -> bool:
    return bool(feedback.review_threads or feedback.comments)


def review_thread_feedback_ref(thread: ReviewThreadFeedback) -> str:
    return f"{thread.thread_id}:{thread.comment_id}"


def followup_top_level_reply_body(
    comment: PullRequestCommentFeedback,
    commit_sha: str,
) -> str:
    return (
        f"Dorf followup addressed comment {comment.comment_id} "
        f"from @{comment.author} in {commit_sha}."
    )


def is_dorf_rest_comment(comment: dict, dorf_app_slug: str) -> bool:
    app = comment.get("performed_via_github_app")
    if isinstance(app, dict) and app.get("slug") == dorf_app_slug:
        return True
    return comment_user_login(comment) == github_app_bot_login(dorf_app_slug)


def is_dorf_graphql_comment(comment: dict, dorf_app_slug: str) -> bool:
    author = comment.get("author")
    login = author.get("login") if isinstance(author, dict) else None
    return login == github_app_bot_login(dorf_app_slug)


def comment_user_login(comment: dict) -> str | None:
    user = comment.get("user")
    login = user.get("login") if isinstance(user, dict) else None
    return login if isinstance(login, str) else None


def github_app_bot_login(app_slug: str) -> str:
    return f"{app_slug}[bot]"


def github_pr_head_sha_or_exit(pr: dict) -> str:
    head = pr.get("head")
    head_sha = head.get("sha") if isinstance(head, dict) else None
    if not isinstance(head_sha, str) or not head_sha:
        raise RuntimeError("GitHub PR response missing head.sha")
    return head_sha


def split_repo_full_name(repo_full_name: str) -> tuple[str, str]:
    parts = repo_full_name.split("/", 1)
    if len(parts) != 2 or not parts[0] or not parts[1]:
        raise RuntimeError(f"invalid GitHub repo: {repo_full_name}")
    return parts[0], parts[1]


def verify_job_readiness(
    store: CodingStore,
    environment: CodingEnvironment,
    job: CodingJob,
    contract: RepoContract,
    *,
    github_client: Callable[[], GitHubRepositoryClient] | None = None,
) -> JobReadiness:
    failures: list[str] = []
    dirty_worktree = False
    binding = store.get_job_binding(job.job_name)
    if binding is None:
        return JobReadiness([f"Job binding not found: {job.job_name}"])

    head = git_output(environment, binding, "rev-parse", "HEAD")
    branch = git_output(environment, binding, "branch", "--show-current")
    status = git_output(environment, binding, "status", "--porcelain")
    base_ancestor = git_output(
        environment,
        binding,
        "merge-base",
        "--is-ancestor",
        job.target_start_sha,
        "HEAD",
    )
    commit_count = git_output(
        environment,
        binding,
        "rev-list",
        "--count",
        f"{job.target_start_sha}..HEAD",
    )
    remote_sha, remote_error = github_remote_head(
        job,
        github_client=github_client,
    )

    if head.returncode != 0:
        failures.append(f"could not read Job HEAD: {git_command_message(head)}")
        return JobReadiness(failures, dirty_worktree=dirty_worktree)
    head_sha = head.stdout.strip()

    if branch.returncode != 0:
        failures.append(f"could not read Job branch: {git_command_message(branch)}")
    elif branch.stdout.strip() != job.job_branch:
        actual_branch = branch.stdout.strip() or "<detached>"
        failures.append(f"Job branch is {actual_branch}, expected {job.job_branch}")

    if status.returncode != 0:
        failures.append(f"could not read Job working tree status: {git_command_message(status)}")
    elif status.stdout.strip():
        dirty_worktree = True
        failures.append("Job working tree is dirty")

    if base_ancestor.returncode != 0:
        failures.append("Job HEAD does not descend from target base")
    elif commit_count.returncode != 0:
        failures.append(f"could not compare Job HEAD to base: {git_command_message(commit_count)}")
    else:
        try:
            commits_beyond_base = int(commit_count.stdout.strip())
        except ValueError:
            commits_beyond_base = 0
        if commits_beyond_base < 1:
            failures.append("Job branch has no commits beyond target base")

    runs = store.list_command_runs(job.job_name)
    for command_name in ("check", "smoke"):
        required_command = contract.commands.get(command_name)
        if required_command is None:
            continue
        latest_head_run = next(
            (
                run
                for run in runs
                if run.kind == command_name
                and run.command == required_command
                and run.git_commit_before == head_sha
                and run.git_commit_after == head_sha
            ),
            None,
        )
        if latest_head_run is None:
            failures.append(f"required {command_name} record did not pass at Job HEAD")
        elif latest_head_run.status != "succeeded" or latest_head_run.exit_code != 0:
            failures.append(f"latest required {command_name} record failed at Job HEAD")
        elif latest_head_run.finished_at is None:
            failures.append(f"latest required {command_name} record did not finish at Job HEAD")

    if remote_error is not None:
        failures.append(f"could not read remote Job branch: {remote_error}")
    elif remote_sha is None:
        failures.append(f"remote Job branch not found: {job.job_branch}")
    elif remote_sha != head_sha:
        repaired = False
        if not failures:
            repaired = publish_job_head_if_ahead(
                environment,
                job,
                binding,
                local_head=head_sha,
                remote_head=remote_sha,
            )
        if not repaired:
            failures.append("remote Job branch does not match Job HEAD")

    return JobReadiness(failures, dirty_worktree=dirty_worktree)


def git_output(
    environment: CodingEnvironment,
    binding: JobBinding,
    *args: str,
) -> subprocess.CompletedProcess[str]:
    return environment.execute(
        ["git", *args],
        cwd=binding.workspace,
        env={"GIT_TERMINAL_PROMPT": "0"},
    )


def git_auth_failed(result: subprocess.CompletedProcess[str]) -> bool:
    message = git_command_message(result).lower()
    return any(
        phrase in message
        for phrase in (
            "authentication failed",
            "could not read username",
            "invalid username or token",
            "terminal prompts disabled",
        )
    )


def git_object_missing(result: subprocess.CompletedProcess[str]) -> bool:
    message = git_command_message(result).lower()
    return any(
        phrase in message
        for phrase in (
            "bad object",
            "invalid object name",
            "not a valid commit name",
            "unknown revision or path not in the working tree",
        )
    )


def publish_job_head_if_ahead(
    environment: CodingEnvironment,
    job: CodingJob,
    binding: JobBinding,
    *,
    local_head: str,
    remote_head: str,
) -> bool:
    if not job_head_is_ahead(environment, binding, remote_head=remote_head):
        return False

    refspec = f"HEAD:refs/heads/{job.job_branch}"
    credentials_refreshed = False
    push = git_output(environment, binding, "push", "origin", refspec)
    if push.returncode != 0 and git_auth_failed(push):
        try:
            environment.refresh_git_credentials()
        except RuntimeError as error:
            raise GitPublicationRepairError(str(error)) from error
        credentials_refreshed = True
        push = git_output(environment, binding, "push", "origin", refspec)
    if push.returncode != 0:
        retry_context = " after refreshing Git credentials" if credentials_refreshed else ""
        raise GitPublicationRepairError(
            f"could not publish Job HEAD{retry_context}: {git_command_message(push)}"
        )

    current_head = git_output(environment, binding, "rev-parse", "HEAD")
    if current_head.returncode != 0:
        raise GitPublicationRepairError(
            f"could not confirm Job HEAD after publication: {git_command_message(current_head)}"
        )
    if current_head.stdout.strip() != local_head:
        raise GitPublicationRepairError(
            "Job HEAD changed while publishing the Job branch; rerun verification"
        )
    return True


def job_head_is_ahead(
    environment: CodingEnvironment,
    binding: JobBinding,
    *,
    remote_head: str,
) -> bool:
    ancestor = git_output(
        environment,
        binding,
        "merge-base",
        "--is-ancestor",
        remote_head,
        "HEAD",
    )
    if ancestor.returncode == 1:
        return False
    if ancestor.returncode != 0 and git_object_missing(ancestor):
        return False
    if ancestor.returncode != 0:
        raise GitPublicationRepairError(
            "could not determine whether Job HEAD is ahead of the remote Job "
            f"branch: {git_command_message(ancestor)}"
        )
    return True


def git_command_message(result: subprocess.CompletedProcess[str]) -> str:
    return result.stderr.strip() or result.stdout.strip() or "command failed"


def github_remote_head(
    job: CodingJob,
    *,
    github_client: Callable[[], GitHubRepositoryClient] | None = None,
) -> tuple[str | None, str | None]:
    repo_full_name = job.metadata.get("github_repo")
    if not repo_full_name:
        return None, "Job metadata is missing github_repo"
    try:
        if github_client is None:
            return None, "GitHub client is unavailable"
        client = github_client()
        return client.get_branch_sha(
            repo_full_name,
            job.job_branch,
        ), None
    except (GitHubAppConfigError, GitHubAppVerificationError, GitHubRepositoryError) as error:
        return None, str(error)


def github_pr_verification_comment(store: CodingStore, job: CodingJob) -> str:
    checklist = store.get_acceptance_checklist(job.job_name)
    binding = store.get_job_binding(job.job_name)
    if checklist is not None and binding is not None:
        return render_proof_dossier(
            build_proof_dossier(
                store,
                job,
                binding,
                commit_sha=proof_dossier_commit(store, job),
            )
        )
    runs = store.list_command_runs(job.job_name)
    failing_runs = [run for run in runs if run.exit_code not in {0, None}]
    review_runs = [run for run in runs if run.kind == "verify-role:diff"]
    fix_runs = [run for run in runs if run.kind == "verify:fix"]
    check_runs = [run for run in runs if run.kind == "check"]
    commit = next(
        (
            run.git_commit_after or run.git_commit_before
            for run in runs
            if run.git_commit_after or run.git_commit_before
        ),
        job.target_start_sha,
    )
    lines = [
        "Dorf verification",
        f"Status: {job.status}",
        f"Branch: {job.job_branch}",
        f"Commit: {commit}",
        f"Checks: {format_run_summary(check_runs)}",
        f"Readiness result: {'passed' if job.status == 'ready' else 'needs human'}",
        f"DeepSeek advisory: {format_run_summary(review_runs)}",
    ]
    attention = _verifier_attention(job)
    if attention is not None and attention.get("status") == "declined":
        lines.append("Missing advisory review: DeepSeek verifier repair was declined")
    if job.status == "needs-human":
        lines.extend(
            [
                "Failure reason: Job needs human review",
                "Last failing run IDs: "
                + (", ".join(str(run.id) for run in failing_runs[:5]) or "unknown"),
                f"Advisory runs used: {len(review_runs)}",
                f"Gate-fix attempts used: {len(fix_runs)}",
                "Inspection commands:",
            ]
        )
        if failing_runs:
            lines.extend(f"- dorf show-run {job.job_name} {run.id}" for run in failing_runs[:5])
        else:
            lines.append(f"- dorf runs {job.job_name}")
    return "\n".join(lines)


def proof_dossier_commit(store: CodingStore, job: CodingJob) -> str:
    if commit := job.metadata.get("proof_commit"):
        return commit
    for run in store.list_command_runs(job.job_name):
        if run.git_commit_before == run.git_commit_after and run.git_commit_after:
            return run.git_commit_after
    return job.target_start_sha


def format_run_summary(runs: list) -> str:
    if not runs:
        return "none recorded"
    latest = runs[0]
    exit_code = "none" if latest.exit_code is None else str(latest.exit_code)
    return f"{latest.kind} run {latest.id} {latest.status} exit {exit_code}"


def github_pr_from_payload(payload: dict[str, object]) -> tuple[int, str] | None:
    number = payload.get("number")
    url = payload.get("html_url")
    if not isinstance(number, int) or not isinstance(url, str) or not url:
        return None
    return number, url


def github_pr_title(job: CodingJob) -> str:
    proposed = job.metadata.get("pr_title")
    normalized_proposed = " ".join((proposed or "").split())
    normalized_fallback = " ".join(job.task.split())
    normalized = normalized_proposed or normalized_fallback
    if not normalized:
        normalized = " ".join(job.job_branch.split()) or "Dorf change"
    return normalized[:256].rstrip() or "Dorf change"


def github_pr_body(job: CodingJob) -> str:
    body = job.metadata.get("pr_body")
    if body is not None and body.strip():
        return body.strip()
    return "\n".join(
        [
            f"Dorf Job: {job.job_name}",
            f"Target branch: {job.target_branch}",
            f"Target start SHA: {job.target_start_sha}",
            f"CodingJob branch: {job.job_branch}",
        ]
    )
