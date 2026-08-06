import json
import sqlite3
import subprocess
from pathlib import Path

import pytest

from dorf.cli import run_repository_preparation_or_raise
from dorf.command_runner import CommandInterrupted, shell_command
from dorf.repo_contract import RepoContract, ReviewAgent, ReviewConfig
from dorf.runtime import (
    AgentConversationInspection,
    ArtifactInput,
    JobRuntime,
    NewJob,
    NewWorker,
    WorkerRuntime,
    WorkerTurnOutcome,
)
from dorf.sdk import JobExecution
from dorf.workflows import (
    AcceptanceItem,
    CodingStore,
    CodingWorkflow,
    WorkflowFailure,
    build_coding_job_pulse,
    run_coding_job_command,
)
from dorf.workflows.coding import verify_job_readiness
from dorf.workflows.coding_admission import CodingAdmissionRequest, GitHubAuthorityApproval
from dorf.workflows.coding_commands import prepare_coding_repository


class Agent:
    agent_type = "codex"

    def __init__(self) -> None:
        self.turns = []
        self.on_continue = None

    def prepare(self, binding) -> None:
        pass

    def start_job_conversation(
        self,
        binding,
        turn,
        *,
        conversation_started,
        turn_prepared,
        turn_started,
    ):
        self.turns.append(turn.prompt)
        conversation_started("thread-job")
        turn_prepared(None)
        turn_started(f"turn-{len(self.turns)}")
        return WorkerTurnOutcome(f"turn-{len(self.turns)}", "completed")

    def continue_job_conversation(self, binding, turn, *, turn_prepared, turn_started):
        self.turns.append(turn.prompt)
        if self.on_continue is not None:
            self.on_continue()
        turn_prepared(f"turn-{len(self.turns) - 1}")
        turn_started(f"turn-{len(self.turns)}")
        return WorkerTurnOutcome(f"turn-{len(self.turns)}", "completed")

    def inspect_job_conversation(self, binding, turns):
        return AgentConversationInspection(
            "restarted",
            {
                "id": binding.conversation.native_conversation_id,
                "status": {"type": "idle"},
                "turns": [
                    {
                        "id": turn.native_turn_id,
                        "items": [{"type": "agentMessage", "text": "done"}],
                    }
                    for turn in turns
                ],
            },
        )


class Environment:
    environment_type = "incus-vm"
    workspace = "/workspace"

    def __init__(self, *, review_outputs: list[str] | None = None) -> None:
        self.review_outputs = iter(review_outputs or [])
        self.processes = []
        self.git_head = "b" * 40
        self.refreshed = []

    def environment_id(self, worker_name):
        return f"dorf-{worker_name}"

    def initial_metadata(self, worker_name):
        return {"template": "dorf-codex"}

    def create(self, binding):
        pass

    def stop(self, binding):
        return "stopped"

    def destroy(self, binding):
        return "deleted"

    def execute(self, binding, argv=None, **kwargs):
        if argv is None:
            argv = binding
            binding = None
        if argv == ["true"]:
            return subprocess.CompletedProcess(argv, 0, "", "")
        if argv[:2] == ["git", "rev-parse"]:
            return subprocess.CompletedProcess(argv, 0, f"{self.git_head}\n", "")
        if argv == ["git", "branch", "--show-current"]:
            return subprocess.CompletedProcess(argv, 0, "dorf/checkout-perf\n", "")
        if argv == ["git", "status", "--porcelain"]:
            return subprocess.CompletedProcess(argv, 0, "", "")
        if argv[:2] == ["git", "merge-base"]:
            return subprocess.CompletedProcess(argv, 0, "", "")
        if argv[:3] == ["git", "rev-list", "--count"]:
            return subprocess.CompletedProcess(argv, 0, "1\n", "")
        if argv[:2] == ["git", "push"]:
            return subprocess.CompletedProcess(argv, 0, "", "")
        return subprocess.CompletedProcess(argv, 0, "", "")

    def process_command(self, binding, argv=None, **kwargs):
        if argv is None:
            argv = binding
            binding = None
        self.processes.append((binding, argv, kwargs))
        if argv[-1].startswith(("reviewer ", "codex exec ")):
            output = next(self.review_outputs)
            return ["bash", "-lc", f"printf '%s' {output!r}"]
        if argv[-1] == "false":
            return ["bash", "-lc", "exit 1"]
        return ["bash", "-lc", "printf 'command output\\n'"]

    def refresh_git_credentials(self):
        self.refreshed.append(True)


def make_coding_job(tmp_path: Path, *, review_outputs=None):
    store = CodingStore.open(tmp_path / "state.sqlite3")
    environment = Environment(review_outputs=review_outputs)
    agent = Agent()
    WorkerRuntime(store, environment, agent).spawn(
        NewWorker(
            "coder-checkout-perf",
            provenance="coding-workflow",
            lifecycle_policy="dedicated",
        )
    )
    runtime = JobRuntime(store, environment, agent)
    binding = runtime.assign(
        NewJob(
            "checkout-perf",
            "coder-checkout-perf",
            "Implement checkout performance",
            "gpt-5.6-sol",
            "high",
        )
    )
    repo = tmp_path / "repo"
    repo.mkdir()
    job = store.create_coding_job(
        job_name="checkout-perf",
        status="active",
        metadata={
            "task": "Improve checkout performance",
            "target_repo": str(repo),
            "target_branch": "main",
            "target_start_sha": "a" * 40,
            "job_branch": "dorf/checkout-perf",
            "github_repo": "example/repo",
        },
    )
    return store, environment, agent, runtime, job, binding


class GitHubClient:
    def __init__(self) -> None:
        self.created = []
        self.comments = []
        self.drafted = []
        self.branch_sha = "b" * 40

    def list_pull_requests_for_branch(self, *args, **kwargs):
        return []

    def create_pull_request(self, repo, **payload):
        self.created.append((repo, payload))
        return {"number": 42, "html_url": "https://github.test/pull/42"}

    def update_pull_request(self, repo, number, **payload):
        return {"number": number, "html_url": "https://github.test/pull/42"}

    def mark_pull_request_ready(self, repo, number):
        pass

    def mark_pull_request_draft(self, repo, number):
        self.drafted.append((repo, number))

    def add_pull_request_comment(self, repo, number, body):
        self.comments.append(body)

    def get_branch_sha(self, repo, branch):
        return self.branch_sha

    def get_pull_request(self, repo, number):
        return {"number": number, "state": "open", "head": {"sha": "b" * 40}}

    def graphql(self, query, variables):
        return {"data": {"repository": {"pullRequest": {"reviewThreads": {"nodes": []}}}}}

    def list_pull_request_comments(self, repo, number):
        return []


class DiffReviewer:
    def __init__(self, store, outputs=None):
        self.store = store
        self.outputs = iter(outputs or ["DORF_REVIEW_NO_FINDINGS"])
        self.commits = []

    def __call__(self, job, commit):
        self.commits.append(commit)
        result = next(self.outputs)
        exit_code, output = result if isinstance(result, tuple) else (0, result)
        path = self.store.database_path.parent / f"diff-{len(self.commits)}.log"
        path.write_text(output)
        run = self.store.create_command_run(
            job.job_name,
            "verify-role:diff",
            f"pi deepseek-v4-flash diff at {commit}",
            str(path),
        )
        run = self.store.finish_command_run(
            run.id, "succeeded" if exit_code == 0 else "failed", exit_code
        )
        return self.store.set_command_run_git_commits(
            run.id,
            before=commit,
            after=commit if exit_code == 0 else None,
        )


def workflow(
    store,
    environment,
    runtime,
    job,
    github,
    *,
    contract=None,
    reviewer=None,
):
    binding = store.get_job_binding(job.job_name)
    assert binding is not None
    return CodingWorkflow(
        store=store,
        job=job,
        contract=contract or RepoContract(mode="generic", commands={}, env={}),
        execution=JobExecution(binding, runtime, environment, Agent(), lambda job: "token"),
        deepseek_diff_review=reviewer or DiffReviewer(store),
        github_client=lambda: github,
        github_app_slug=lambda: "dorf-test",
        sleep=lambda seconds: None,
    )


def add_recovery_required_job_turn(store, runtime, job_name: str) -> None:
    initial = store.list_job_inputs(job_name)[0]
    if store.get_job_turn_by_input(job_name, initial.id) is None:
        assert runtime.deliver_input(job_name, initial.id).status == "succeeded"
    job_input, _ = runtime.admit_message(
        job_name,
        message_id="jmsg-recovery-required",
        text="Repair interrupted work",
    )
    turn, created = store.admit_job_turn(job_input, output_path="/tmp/recovery.log")
    assert created
    store.prepare_job_turn(turn.id, baseline_native_turn_id="turn-baseline")
    store.start_job_turn(turn.id, "turn-recovery")
    store.finish_job_turn(
        turn.id,
        status="recovery-required",
        exit_code=75,
        error="controller disconnected",
    )


def test_coding_store_has_no_superseded_session_or_duplicate_turn_tables(tmp_path) -> None:
    store, *_ = make_coding_job(tmp_path)

    names = {
        row[0]
        for row in sqlite3.connect(store.database_path).execute(
            "SELECT name FROM sqlite_master WHERE type = 'table'"
        )
    }

    assert {"coding_jobs", "coding_command_runs"} <= names
    assert not {"sessions", "job_messages", "command_runs"} & names
    coding_columns = {
        row[1]
        for row in sqlite3.connect(store.database_path).execute("PRAGMA table_info(coding_jobs)")
    }
    assert not {"worker_name", "room_id", "assignment_id", "conversation_id"} & coding_columns


def test_pending_github_authority_attempt_is_retry_safe_and_approved_idempotently(
    tmp_path,
) -> None:
    store = CodingStore.open(tmp_path / "state.sqlite3")
    request = CodingAdmissionRequest(
        repo_path=str(tmp_path / "repo"),
        target_branch="main",
        issue_number=20,
        repository="example/repo",
        installation_id="123",
        provider_connection="personal-chatgpt",
    )
    approval = GitHubAuthorityApproval(
        installation_id="123",
        missing_authority="Dorf GitHub App access to example/repo",
        why_needed="Read the issue and publish the coding proposal.",
        action="Add example/repo to installation 123.",
        scope="Only example/repo.",
        approve_consequence="The coding Job can be admitted.",
        decline_consequence="No Job or GitHub resource is created.",
        automatic_resume="Exact readiness reruns and the delegation continues.",
        url="https://github.com/settings/installations/123",
        repository="example/repo",
    )

    first, created = store.retain_pending_coding_admission(
        request.record(), approval.record(), ttl_seconds=3600
    )
    retried, retry_created = store.retain_pending_coding_admission(
        request.record(), approval.record(), ttl_seconds=3600
    )

    assert created is True
    assert retry_created is False
    assert retried == first
    assert store.approve_coding_admission(first.id) is True
    assert store.approve_coding_admission(first.id) is True
    attempts = store.list_coding_admissions()
    assert len(attempts) == 1
    assert attempts[0].status == "approved"
    assert attempts[0].request == request.record()
    assert "token" not in str(attempts[0].request).lower()
    acceptance = (
        AcceptanceItem("goal-1", "Publish issue #20", "issue", "manual", "issue:20"),
    )

    with pytest.raises(RuntimeError, match="installation identity does not match"):
        store.create_coding_job_with_acceptance(
            job_name="wrong-installation",
            metadata={
                "admission_proof": (
                    '{"proof_id":"proof-1","installation_id":"456"}'
                )
            },
            goal="Issue #20",
            items=acceptance,
            admission_attempt_id=first.id,
        )
    assert store.get_coding_admission(first.id).status == "approved"
    assert store.get_coding_job("wrong-installation") is None
    assert store.get_acceptance_checklist("wrong-installation") is None

    store.create_coding_job_with_acceptance(
        job_name="approved-job",
        metadata={
            "admission_proof": '{"proof_id":"proof-1","installation_id":"123"}'
        },
        goal="Issue #20",
        items=acceptance,
        admission_attempt_id=first.id,
    )
    with pytest.raises(RuntimeError, match="already been consumed"):
        store.create_coding_job_with_acceptance(
            job_name="duplicate-job",
            metadata={
                "admission_proof": '{"proof_id":"proof-1","installation_id":"123"}'
            },
            goal="Issue #20",
            items=acceptance,
            admission_attempt_id=first.id,
        )

    admitted = store.get_coding_admission(first.id)
    assert admitted.status == "admitted"
    assert admitted.job_name == "approved-job"
    assert admitted.proof_id == "proof-1"
    assert [job.job_name for job in store.list_coding_jobs()] == ["approved-job"]
    assert store.get_acceptance_checklist("approved-job").items == acceptance
    assert store.get_coding_job("duplicate-job") is None
    assert store.get_acceptance_checklist("duplicate-job") is None


def test_retain_pending_github_authority_expires_stale_attempt_before_reuse(
    tmp_path,
) -> None:
    store = CodingStore.open(tmp_path / "state.sqlite3")
    request = CodingAdmissionRequest(
        repo_path=str(tmp_path / "repo"),
        target_branch="main",
        issue_number=20,
        repository="example/repo",
        installation_id="123",
    )
    approval = GitHubAuthorityApproval(
        installation_id="123",
        missing_authority="Dorf GitHub App access to example/repo",
        why_needed="Read the issue and publish the proposal.",
        action="Add example/repo to installation 123.",
        scope="Only example/repo.",
        approve_consequence="The coding Job can be admitted.",
        decline_consequence="No resources are created.",
        automatic_resume="Exact readiness reruns automatically.",
        url="https://github.com/settings/installations/123",
        repository="example/repo",
    )
    stale, _ = store.retain_pending_coding_admission(
        request.record(), approval.record(), ttl_seconds=-1
    )

    renewed, created = store.retain_pending_coding_admission(
        request.record(), approval.record(), ttl_seconds=3600
    )

    assert created is True
    assert renewed.id != stale.id
    assert store.get_coding_admission(stale.id).status == "expired"
    assert renewed.status == "pending"


def test_expired_approved_github_authority_cannot_be_consumed(tmp_path) -> None:
    store = CodingStore.open(tmp_path / "state.sqlite3")
    request = CodingAdmissionRequest(
        repo_path=str(tmp_path / "repo"),
        target_branch="main",
        issue_number=20,
        repository="example/repo",
        installation_id="123",
    )
    approval = GitHubAuthorityApproval(
        installation_id="123",
        missing_authority="Dorf GitHub App access to example/repo",
        why_needed="Read the issue and publish the proposal.",
        action="Add example/repo to installation 123.",
        scope="Only example/repo.",
        approve_consequence="The coding Job can be admitted.",
        decline_consequence="No resources are created.",
        automatic_resume="Exact readiness reruns automatically.",
        url="https://github.com/settings/installations/123",
        repository="example/repo",
    )
    attempt, _ = store.retain_pending_coding_admission(
        request.record(), approval.record(), ttl_seconds=3600
    )
    assert store.approve_coding_admission(attempt.id) is True
    with sqlite3.connect(store.database_path) as connection:
        connection.execute(
            "UPDATE coding_admissions SET expires_at = ? WHERE id = ?",
            ("2000-01-01T00:00:00.000000+00:00", attempt.id),
        )

    with pytest.raises(RuntimeError, match="already been consumed"):
        store.create_coding_job(
            job_name="stale-approval",
            metadata={
                "admission_proof": (
                    '{"proof_id":"proof-stale","installation_id":"123"}'
                )
            },
            admission_attempt_id=attempt.id,
        )

    assert store.get_coding_admission(attempt.id).status == "expired"
    assert store.get_coding_job("stale-approval") is None


@pytest.mark.parametrize("outcome", ["declined", "expired"])
def test_decline_or_expiry_ends_pending_authority_without_active_state(
    tmp_path, outcome
) -> None:
    store = CodingStore.open(tmp_path / f"{outcome}.sqlite3")
    request = CodingAdmissionRequest(
        repo_path=str(tmp_path / "repo"),
        target_branch="main",
        issue_number=20,
        repository="example/repo",
        installation_id="123",
    )
    approval = GitHubAuthorityApproval(
        installation_id="123",
        missing_authority="Dorf GitHub App access to example/repo",
        why_needed="Read the issue and publish the proposal.",
        action="Add example/repo to installation 123.",
        scope="Only example/repo.",
        approve_consequence="The coding Job can be admitted.",
        decline_consequence="No resources are created.",
        automatic_resume="Exact readiness reruns automatically.",
        url="https://github.com/settings/installations/123",
        repository="example/repo",
    )
    attempt, _ = store.retain_pending_coding_admission(
        request.record(), approval.record(), ttl_seconds=3600
    )

    assert store.end_pending_coding_admission(attempt.id, outcome) is True
    assert store.end_pending_coding_admission(attempt.id, outcome) is False
    assert store.get_coding_admission(attempt.id).status == outcome
    assert store.list_coding_jobs() == []
    assert sqlite3.connect(store.database_path).execute(
        "SELECT COUNT(*) FROM workers"
    ).fetchone()[0] == 0

    renewed, created = store.retain_pending_coding_admission(
        request.record(), approval.record(), ttl_seconds=3600
    )

    assert created is True
    assert renewed.id != attempt.id
    assert renewed.status == "pending"
    assert [item.status for item in store.list_coding_admissions()] == [
        outcome,
        "pending",
    ]


def test_coding_command_runs_in_assignment_workspace_and_records_fact_evidence(
    tmp_path,
) -> None:
    store, environment, agent, runtime, job, binding = make_coding_job(tmp_path)
    execution = JobExecution(binding, runtime, environment, agent, lambda job: "token")

    run = run_coding_job_command(
        store,
        execution,
        job,
        binding,
        RepoContract(mode="generic", commands={}, env={}),
        shell_command("check", "true"),
    )

    assert run.status == "succeeded"
    assert run.git_commit_before == run.git_commit_after == "b" * 40
    process_binding, _argv, options = environment.processes[-1]
    assert process_binding == binding
    assert options["cwd"] == "/workspace/jobs/checkout-perf"
    assert options["env"]["DORF_JOB_NAME"] == "checkout-perf"
    assert options["env"]["DORF_ASSIGNMENT_ID"] == binding.assignment.id
    event = store.documents.list_events("checkout-perf")[-1]
    assert (event.source, event.provenance, event.kind) == (
        "workflow",
        "fact",
        "command-result",
    )
    assert event.related["assignment"] == binding.assignment.id
    assert event.artifacts[0].name == "check-output.log"


def test_repository_preparation_is_a_recorded_repo_owned_command(tmp_path) -> None:
    store, environment, _agent, _runtime, job, binding = make_coding_job(tmp_path)
    contract = RepoContract(
        mode="configured",
        commands={"prepare": "uv sync --frozen"},
        env={},
    )

    run = prepare_coding_repository(store, environment, job, binding, contract)

    assert run is not None
    assert (run.kind, run.command, run.status) == (
        "prepare",
        "uv sync --frozen",
        "succeeded",
    )
    assert environment.processes[-1][2]["cwd"] == binding.workspace


def test_repository_preparation_is_optional_for_generic_repositories(tmp_path) -> None:
    store, environment, _agent, _runtime, job, binding = make_coding_job(tmp_path)

    run = prepare_coding_repository(
        store,
        environment,
        job,
        binding,
        RepoContract(mode="generic", commands={}, env={}),
    )

    assert run is None
    assert environment.processes == []


def test_failed_repository_preparation_stops_before_an_agent_turn(tmp_path) -> None:
    store, environment, agent, _runtime, _job, binding = make_coding_job(tmp_path)
    contract = RepoContract(
        mode="configured",
        commands={"prepare": "false"},
        env={},
    )

    with pytest.raises(RuntimeError, match="repository preparation exited with code 1"):
        run_repository_preparation_or_raise(store, environment, binding, contract)

    assert agent.turns == []


def test_readiness_uses_current_assignment_and_passing_command_at_head(tmp_path) -> None:
    store, environment, _agent, _runtime, job, binding = make_coding_job(tmp_path)
    store.record_command_run(
        job.job_name,
        "check",
        "check-command",
        "succeeded",
        0,
        git_commit_before="b" * 40,
        git_commit_after="b" * 40,
    )

    readiness = verify_job_readiness(
        store,
        environment,
        job,
        RepoContract(mode="configured", commands={"check": "check-command"}, env={}),
        github_client=lambda: GitHubClient(),
    )

    assert readiness.failures == []
    assert store.get_job_binding(job.job_name) == binding


def test_readiness_refreshes_credentials_on_current_assignment_and_retries_push(
    tmp_path,
) -> None:
    store, environment, _agent, _runtime, job, binding = make_coding_job(tmp_path)
    pushes = 0
    original_execute = environment.execute

    def execute(actual_binding, argv=None, **kwargs):
        nonlocal pushes
        if argv is None:
            argv = actual_binding
            actual_binding = None
        else:
            assert actual_binding == binding
        if argv[:2] == ["git", "push"]:
            pushes += 1
            if pushes == 1:
                return subprocess.CompletedProcess(argv, 1, "", "Authentication failed")
        return original_execute(actual_binding, argv, **kwargs)

    environment.execute = execute
    class BehindGitHub(GitHubClient):
        def get_branch_sha(self, repo, branch):
            return "c" * 40

    readiness = verify_job_readiness(
        store,
        environment,
        job,
        RepoContract(mode="generic", commands={}, env={}),
        github_client=lambda: BehindGitHub(),
    )

    assert readiness.failures == []
    assert environment.refreshed == [True]
    assert pushes == 2


def test_check_command_does_not_receive_provider_route_credential(tmp_path) -> None:
    store, environment, _agent, _runtime, job, binding = make_coding_job(tmp_path)

    run_coding_job_command(
        store,
        environment,
        job,
        binding,
        RepoContract(mode="configured", commands={"check": "env"}, env={}),
        shell_command("check", "env"),
    )

    assert environment.processes[-1][2]["provider_route"] is False


def test_followup_without_new_feedback_reads_current_assignment_and_keeps_job_open(
    tmp_path,
) -> None:
    store, environment, _agent, runtime, job, binding = make_coding_job(tmp_path)
    store.record_github_pr(job.job_name, 42, "https://github.test/pull/42")
    store.update_status(job.job_name, "ready")
    job = store.get_coding_job(job.job_name)
    assert job is not None

    outcome = workflow(store, environment, runtime, job, GitHubClient()).followup()

    assert outcome.messages[-1].text == "No new PR feedback for checkout-perf."
    assert store.get_job(job.job_name).status == "open"
    assert store.get_job_binding(job.job_name) == binding


def test_verify_publishes_ready_pr_without_ending_job_or_worker(tmp_path) -> None:
    store, environment, _agent, runtime, job, _binding = make_coding_job(tmp_path)
    github = GitHubClient()
    contract = RepoContract(
        mode="configured",
        commands={},
        env={},
        review=ReviewConfig(
            max_rounds=1,
            agents={"codex": ReviewAgent("codex", "codex exec {dorf_review_prompt}")},
        ),
    )

    outcome = workflow(store, environment, runtime, job, github, contract=contract).verify()

    assert outcome.messages[-1].text == "Verify passed for checkout-perf"
    assert store.get_coding_job("checkout-perf").status == "ready"
    assert store.get_coding_job("checkout-perf").github_pr_number == 42
    assert store.get_job("checkout-perf").status == "open"
    assert store.get_worker("coder-checkout-perf").status == "assigned"
    assert not any("codex exec" in item[1][-1] for item in environment.processes)


def test_verify_freezes_and_requires_commit_pinned_acceptance_before_ready(tmp_path) -> None:
    store, environment, _agent, runtime, job, binding = make_coding_job(tmp_path)
    contract = RepoContract(
        mode="configured",
        commands={"check": "true", "smoke": "true"},
        env={},
    )
    store.record_acceptance_checklist(
        job.job_name,
        goal=binding.job.goal,
        items=(
            AcceptanceItem(
                "issue-1",
                "Behavior is correct",
                "issue",
                "command",
                "check",
                "true",
            ),
            AcceptanceItem(
                "repo-check", "Checks pass", "contract", "command", "check", "true"
            ),
            AcceptanceItem(
                "repo-smoke", "Smoke passes", "contract", "command", "smoke", "true"
            ),
        ),
    )
    github = GitHubClient()

    workflow(store, environment, runtime, job, github, contract=contract).verify()

    accepted = store.get_acceptance_checklist(job.job_name)
    ready = store.get_coding_job(job.job_name)
    assert accepted.state == "governing"
    assert ready.status == "ready"
    assert ready.metadata["proof_commit"] == "b" * 40
    assert {run.kind for run in store.list_command_runs(job.job_name)} >= {
        "check",
        "smoke",
        "verify-role:diff",
    }
    assert "# Dorf proof dossier · checkout-perf" in github.comments[0]
    assert "## Acceptance status" in github.comments[0]
    assert "`bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb`" in github.comments[0]


def test_mark_ready_freezes_acceptance_before_a_failed_readiness_attempt(tmp_path) -> None:
    store, environment, _agent, runtime, job, _binding = make_coding_job(tmp_path)
    store.record_acceptance_checklist(
        job.job_name,
        goal="Pinned goal",
        items=(AcceptanceItem("manual", "Human check", "human", "manual", ""),),
    )
    original_execute = environment.execute

    def execute(binding, argv, **kwargs):
        if argv == ["git", "status", "--porcelain"]:
            return subprocess.CompletedProcess(argv, 0, "dirty.txt\n", "")
        return original_execute(binding, argv, **kwargs)

    environment.execute = execute

    with pytest.raises(WorkflowFailure):
        workflow(store, environment, runtime, job, GitHubClient()).mark_ready()

    assert store.get_acceptance_checklist(job.job_name).state == "governing"


def test_afk_takeover_interrupts_an_abandoned_smoke_run(tmp_path) -> None:
    store, _environment, _agent, _runtime, job, _binding = make_coding_job(tmp_path)
    run = store.create_command_run(job.job_name, "smoke", "smoke-command", "")

    interrupted = store.interrupt_abandoned_afk_runs(job.job_name)

    assert [item.id for item in interrupted] == [run.id]
    assert store.get_command_run(run.id).status == "interrupted"


def test_verify_reports_unrecovered_job_turn_without_accessing_session_fields(tmp_path) -> None:
    store, environment, _agent, runtime, job, _binding = make_coding_job(tmp_path)
    add_recovery_required_job_turn(store, runtime, job.job_name)
    contract = RepoContract(mode="generic", commands={}, env={})

    with pytest.raises(WorkflowFailure) as raised:
        workflow(store, environment, runtime, job, GitHubClient(), contract=contract).verify()

    assert raised.value.kind == "needs-human"
    assert "unrecovered Job turn" in raised.value.messages[-1].text
    assert store.get_coding_job(job.job_name).status == "needs-human"
    assert store.get_job(job.job_name).status == "open"


def test_followup_reports_unrecovered_job_turn_as_controlled_outcome(tmp_path) -> None:
    store, environment, _agent, runtime, job, _binding = make_coding_job(tmp_path)
    add_recovery_required_job_turn(store, runtime, job.job_name)
    store.update_status(job.job_name, "ready")
    store.record_github_pr(job.job_name, 42, "https://github.test/pull/42")
    job = store.get_coding_job(job.job_name)
    assert job is not None

    with pytest.raises(WorkflowFailure) as raised:
        workflow(store, environment, runtime, job, GitHubClient()).followup()

    assert raised.value.kind == "needs-human"
    assert "unrecovered Job turn" in raised.value.messages[-1].text
    assert store.get_coding_job(job.job_name).status == "needs-human"
    assert store.get_job(job.job_name).status == "open"


def test_review_finding_repairs_through_same_job_fifo_then_rechecks(tmp_path) -> None:
    finding = "- [P1] Cover the regression"
    clean = "DORF_REVIEW_NO_FINDINGS"
    store, environment, agent, runtime, job, _binding = make_coding_job(tmp_path)
    initial = store.list_job_inputs(job.job_name)[0]
    assert runtime.deliver_input(job.job_name, initial.id).status == "succeeded"
    github = GitHubClient()
    contract = RepoContract(mode="configured", commands={"check": "true"}, env={})
    reviewer = DiffReviewer(store, [finding, clean])

    def commit_decision():
        environment.git_head = "c" * 40
        github.branch_sha = environment.git_head

    agent.on_continue = commit_decision

    workflow(
        store,
        environment,
        runtime,
        job,
        github,
        contract=contract,
        reviewer=reviewer,
    ).verify()

    inputs = store.list_job_inputs(job.job_name)
    assert [item.sequence for item in inputs] == [1, 2]
    assert inputs[1].kind == "message"
    assert "DeepSeek diff advisory findings" in inputs[1].text
    assert len(agent.turns) == 2
    assert reviewer.commits == ["b" * 40, "c" * 40]
    assert sum(item[1][-1] == "true" for item in environment.processes) == 2
    assert store.get_job(job.job_name).status == "open"


def test_second_deepseek_findings_stop_after_one_fifo_repair(tmp_path) -> None:
    finding = "- [P1] Still worth reconsidering"
    store, environment, agent, runtime, job, _binding = make_coding_job(tmp_path)
    initial = store.list_job_inputs(job.job_name)[0]
    assert runtime.deliver_input(job.job_name, initial.id).status == "succeeded"
    github = GitHubClient()
    reviewer = DiffReviewer(store, [finding, finding])

    def commit_decision():
        environment.git_head = "c" * 40
        github.branch_sha = environment.git_head

    agent.on_continue = commit_decision

    with pytest.raises(WorkflowFailure) as raised:
        workflow(store, environment, runtime, job, github, reviewer=reviewer).verify()

    assert raised.value.kind == "needs-human"
    assert reviewer.commits == ["b" * 40, "c" * 40]
    assert len(store.list_job_inputs(job.job_name)) == 2


def test_deepseek_result_must_match_the_implementation_commit(tmp_path) -> None:
    store, environment, _agent, runtime, job, _binding = make_coding_job(tmp_path)
    reviewer = DiffReviewer(store)

    def mismatched(current, commit):
        run = reviewer(current, commit)
        return store.set_command_run_git_commits(
            run.id, before=commit, after="d" * 40
        )

    with pytest.raises(WorkflowFailure, match="not pinned"):
        workflow(
            store,
            environment,
            runtime,
            job,
            GitHubClient(),
            reviewer=mismatched,
        ).verify()


def test_clean_deepseek_verdict_survives_loss_of_the_mutable_run_log(tmp_path) -> None:
    store, environment, _agent, runtime, job, _binding = make_coding_job(tmp_path)
    github = GitHubClient()
    reviewer = DiffReviewer(store)

    workflow(store, environment, runtime, job, github, reviewer=reviewer).verify()
    run = next(
        item for item in store.list_command_runs(job.job_name)
        if item.kind == "verify-role:diff"
    )
    Path(run.output_path).unlink()
    current = store.get_coding_job(job.job_name)

    workflow(store, environment, runtime, current, github, reviewer=reviewer).verify()

    assert reviewer.commits == ["b" * 40]
    verdicts = [
        event for event in store.documents.list_events(job.job_name)
        if event.kind == "review-verdict"
    ]
    assert len(verdicts) == 1


def test_afk_attention_decision_waits_for_coordinator_ownership(tmp_path) -> None:
    store, _environment, _agent, _runtime, job, _binding = make_coding_job(tmp_path)
    repo = str(Path(job.target_repo).resolve())
    attention = {"id": "attention-exact", "status": "outstanding"}
    store.set_metadata_values(
        job.job_name,
        {
            "afk_issue_number": "139",
            "diff_verifier_attention": json.dumps(attention),
        },
    )
    store.claim_afk_coordinator(repo, 139, "current-owner")
    store.link_afk_job(repo, 139, "current-owner", job.job_name)

    with pytest.raises(WorkflowFailure) as raised:
        CodingWorkflow.prepare_afk_resume(
            store,
            job_name=job.job_name,
            owner_token="other-owner",
            takeover=False,
            repair_attention_id=attention["id"],
        )

    assert raised.value.kind == "ownership"
    current = store.get_coding_job(job.job_name)
    assert json.loads(current.metadata["diff_verifier_attention"])["status"] == "outstanding"


def test_interrupted_deepseek_run_stays_interrupted_without_consuming_attention(
    tmp_path,
) -> None:
    store, environment, _agent, runtime, job, _binding = make_coding_job(tmp_path)

    def interrupted(current, commit):
        run = store.create_command_run(
            current.job_name, "verify-role:diff", "pi deepseek-v4-flash", ""
        )
        run = store.finish_command_run(run.id, "interrupted", 130)
        store.set_command_run_git_commits(run.id, before=commit, after=None)
        raise CommandInterrupted(run)

    with pytest.raises(WorkflowFailure) as raised:
        workflow(
            store,
            environment,
            runtime,
            job,
            GitHubClient(),
            reviewer=interrupted,
        ).verify()

    assert raised.value.kind == "interrupted"
    current = store.get_coding_job(job.job_name)
    assert "diff_verifier_attention" not in current.metadata

    reviewer = DiffReviewer(store)
    workflow(
        store,
        environment,
        runtime,
        current,
        GitHubClient(),
        reviewer=reviewer,
    ).verify()
    assert reviewer.commits == ["b" * 40]
    assert "diff_verifier_attention" not in store.get_coding_job(job.job_name).metadata


def test_verifier_failure_deduplicates_attention_and_repair_clears_afk_projection(
    tmp_path,
) -> None:
    store, environment, _agent, runtime, job, _binding = make_coding_job(tmp_path)
    github = GitHubClient()
    reviewer = DiffReviewer(
        store,
        [(1, "provider gateway authentication unavailable"), "DORF_REVIEW_NO_FINDINGS"],
    )

    with pytest.raises(WorkflowFailure) as first:
        workflow(store, environment, runtime, job, github, reviewer=reviewer).verify()
    assert first.value.kind == "verifier-attention"
    blocked = store.get_coding_job(job.job_name)
    attention = json.loads(blocked.metadata["diff_verifier_attention"])
    assert attention["status"] == "outstanding"
    assert len(reviewer.commits) == 1

    with pytest.raises(WorkflowFailure) as repeated:
        workflow(store, environment, runtime, blocked, github, reviewer=reviewer).verify()
    assert repeated.value.kind == "verifier-attention"
    assert len(reviewer.commits) == 1

    workflow(
        store,
        environment,
        runtime,
        blocked,
        github,
        reviewer=reviewer,
    ).verify(repair_attention_id=attention["id"])

    ready = store.get_coding_job(job.job_name)
    assert ready.status == "ready"
    assert len(reviewer.commits) == 2
    assert not {
        "diff_verifier_attention",
        "afk_stage",
        "afk_outcome",
    }.intersection(ready.metadata)


def test_declined_verifier_attention_keeps_draft_without_review_evidence(tmp_path) -> None:
    store, environment, _agent, runtime, job, _binding = make_coding_job(tmp_path)
    github = GitHubClient()
    reviewer = DiffReviewer(store, [(1, "DeepSeek route unavailable")])

    with pytest.raises(WorkflowFailure):
        workflow(store, environment, runtime, job, github, reviewer=reviewer).verify()
    blocked = store.get_coding_job(job.job_name)
    attention = json.loads(blocked.metadata["diff_verifier_attention"])

    with pytest.raises(WorkflowFailure) as declined:
        workflow(store, environment, runtime, blocked, github, reviewer=reviewer).verify(
            decline_attention_id=attention["id"]
        )

    current = store.get_coding_job(job.job_name)
    assert declined.value.kind == "needs-human"
    assert current.status == "needs-human"
    assert json.loads(current.metadata["diff_verifier_attention"])["status"] == "declined"
    assert len(reviewer.commits) == 1
    assert github.drafted
    assert "Missing advisory review" in github.comments[-1]


def test_afk_interrupted_setting_up_reservation_requires_setup_resume(tmp_path) -> None:
    store = CodingStore.open(tmp_path / "state.sqlite3")
    repo = tmp_path / "repo"
    repo.mkdir()
    job = store.create_coding_job(
        job_name="reserved-afk",
        status="setting-up",
        metadata={
            "target_repo": str(repo),
            "afk_issue_number": "139",
        },
    )
    store.claim_afk_coordinator(str(repo.resolve()), 139, "owner")
    store.link_afk_job(str(repo.resolve()), 139, "owner", job.job_name)

    with pytest.raises(WorkflowFailure) as raised:
        CodingWorkflow.prepare_afk_start(
            store,
            target_repo=str(repo.resolve()),
            issue_number=139,
            owner_token="owner",
        )

    assert raised.value.kind == "setup"
    assert "afk-resume reserved-afk" in raised.value.messages[0].text


def test_afk_composes_the_same_job_assignment_through_ready_pr(tmp_path) -> None:
    store, environment, _agent, runtime, job, binding = make_coding_job(tmp_path)
    initial = store.list_job_inputs(job.job_name)[0]
    assert runtime.deliver_input(job.job_name, initial.id).status == "succeeded"
    store.claim_afk_coordinator("example/repo", 139, "owner")
    store.link_afk_job("example/repo", 139, "owner", job.job_name)
    contract = RepoContract(
        mode="configured",
        commands={},
        env={},
        review=ReviewConfig(
            max_rounds=1,
            agents={"codex": ReviewAgent("codex", "codex exec {dorf_review_prompt}")},
        ),
    )
    reviewer = DiffReviewer(store)

    outcome = workflow(
        store,
        environment,
        runtime,
        job,
        GitHubClient(),
        contract=contract,
        reviewer=reviewer,
    ).coordinate_afk(issue_number=139, target_repo="example/repo", owner_token="owner")

    assert outcome.messages[-1].text == "AFK complete for checkout-perf"
    assert store.get_afk_coordinator("example/repo", 139).status == "ready"
    afk_run = next(run for run in store.list_command_runs(job.job_name) if run.kind == "afk")
    assert afk_run.status == "succeeded"
    current = store.get_job_binding(job.job_name)
    assert current.assignment.id == binding.assignment.id
    assert current.worker.name == binding.worker.name
    assert current.room.id == binding.room.id
    assert store.get_job(job.job_name).status == "open"
    assert reviewer.commits == ["b" * 40]
    assert not any("codex exec" in item[1][-1] for item in environment.processes)


def test_afk_pulse_keeps_detached_activity_when_coordinator_disappears(tmp_path) -> None:
    store, _environment, _agent, runtime, job, binding = make_coding_job(tmp_path)
    goal = store.list_job_inputs(job.job_name)[0]
    turn, _created = store.admit_job_turn(goal, output_path="/tmp/implementation.log")
    store.prepare_job_turn(turn.id, baseline_native_turn_id=None)
    store.start_job_turn(turn.id, "turn-detached")
    store.set_metadata_values(
        job.job_name,
        {"afk_stage": "implementation", "afk_outcome": "waiting for primary agent"},
    )
    (tmp_path / "implementation.txt").write_text("work in progress\n")
    store.documents.append_event(
        job.job_name,
        event_id="report-progress",
        source="worker",
        provenance="claim",
        kind="progress",
        summary="Implementing the requested change",
        related={"assignment": binding.assignment.id},
        artifacts=[
            ArtifactInput(
                "implementation.txt",
                tmp_path / "implementation.txt",
                "text/plain",
            )
        ],
    )
    store.claim_afk_coordinator("example/repo", 19, "coordinator")
    store.link_afk_job("example/repo", 19, "coordinator", job.job_name)
    inspection = runtime.inspect(job.job_name)

    before = build_coding_job_pulse(store, inspection)
    store.finish_afk_coordinator("example/repo", 19, "coordinator", "lost")
    after = build_coding_job_pulse(store, inspection)

    assert after == before
    assert after.job == "checkout-perf"
    assert after.goal == "Implement checkout performance"
    assert after.outcome_stage == "implementation"
    assert after.observed_activity.status == "active"
    assert after.observed_activity.claim_support == "consistent"
    assert after.worker_claim is not None
    assert after.worker_claim.provenance == "claim"
    assert after.worker_claim.assignment_id == binding.assignment.id
    assert after.evidence_count == 1
    assert "coordinator" not in repr(after)


def test_afk_pulse_keeps_silence_quiet_instead_of_treating_coordinator_as_progress(
    tmp_path,
) -> None:
    store, _environment, _agent, runtime, job, _binding = make_coding_job(tmp_path)
    store.set_metadata_values(
        job.job_name,
        {"afk_stage": "implementation", "afk_outcome": "waiting for primary agent"},
    )
    store.claim_afk_coordinator("example/repo", 19, "coordinator")
    store.link_afk_job("example/repo", 19, "coordinator", job.job_name)

    pulse = build_coding_job_pulse(store, runtime.inspect(job.job_name))

    assert pulse.observed_activity.status == "quiet"
    assert pulse.observed_activity.detail == "No Job turn or workflow command is active"
    assert pulse.attention.state == "quiet"

    store.create_command_run(job.job_name, "check", "pytest", "/tmp/check.log")
    recorded_only = build_coding_job_pulse(store, runtime.inspect(job.job_name))

    assert recorded_only.observed_activity.status == "unconfirmed"
    assert recorded_only.observed_activity.provenance == "fact"
    assert "recorded running" in recorded_only.observed_activity.detail


def test_afk_pulse_preserves_room_unavailability_without_infrastructure_identity(
    tmp_path,
) -> None:
    store, _environment, _agent, runtime, job, binding = make_coding_job(tmp_path)
    store.update_room_status(
        binding.room.id,
        "failed",
        f"Incus VM {binding.room.provider_id} stopped",
    )

    pulse = build_coding_job_pulse(store, runtime.inspect(job.job_name))

    assert pulse.room_availability.status == "unavailable"
    assert pulse.room_availability.detail == "Incus VM Room stopped"
    assert pulse.room_availability.source == "runtime"
    assert pulse.room_availability.provenance == "fact"
    assert binding.room.id not in repr(pulse)
    assert binding.room.provider_id not in repr(pulse)


def test_afk_pulse_interrupted_turn_requires_attention_like_runtime_wait(tmp_path) -> None:
    store, _environment, _agent, runtime, job, _binding = make_coding_job(tmp_path)
    goal = store.list_job_inputs(job.job_name)[0]
    turn, _created = store.admit_job_turn(goal, output_path="/tmp/interrupted.log")
    store.prepare_job_turn(turn.id, baseline_native_turn_id=None)
    store.start_job_turn(turn.id, "turn-interrupted")
    store.finish_job_turn(
        turn.id,
        status="interrupted",
        exit_code=130,
        error="Detached turn was interrupted",
    )

    pulse = build_coding_job_pulse(store, runtime.inspect(job.job_name))

    assert pulse.attention.state == "needs-human"
    assert pulse.attention.reason == "Latest Job input is interrupted"


def test_afk_pulse_ready_outcome_supersedes_later_stale_needs_human_marker(tmp_path) -> None:
    store, _environment, _agent, runtime, job, _binding = make_coding_job(tmp_path)
    store.update_status(job.job_name, "ready")
    store.set_metadata_values(
        job.job_name,
        {"afk_stage": "needs-human", "afk_outcome": "verify requires human"},
    )

    pulse = build_coding_job_pulse(store, runtime.inspect(job.job_name))

    assert pulse.outcome_stage == "ready"
    assert pulse.attention.state == "quiet"
    assert pulse.latest_delta.summary == "Coding outcome is ready"
    assert pulse.latest_delta.source == "workflow"
    assert pulse.latest_delta.provenance == "fact"


@pytest.mark.parametrize("terminal_status", ["merged", "rejected", "abandoned"])
def test_afk_pulse_terminal_state_overrides_stale_attention(tmp_path, terminal_status) -> None:
    store, _environment, _agent, runtime, job, _binding = make_coding_job(tmp_path)
    store.set_metadata_values(
        job.job_name,
        {"afk_stage": "needs-human", "afk_outcome": "verification needs a decision"},
    )
    store.update_status(job.job_name, terminal_status)

    pulse = build_coding_job_pulse(store, runtime.inspect(job.job_name))

    assert pulse.outcome_stage == terminal_status
    assert pulse.latest_delta.summary == f"Coding outcome is {terminal_status}"
    assert pulse.latest_delta.source == "workflow"
    assert pulse.lifecycle.state == "open"
    assert pulse.lifecycle.source == "runtime"
    assert pulse.attention.state == "none"
    assert pulse.attention.reason == f"Job is terminal: {terminal_status}"


def test_afk_pulse_ended_runtime_job_overrides_stale_active_workflow_state(tmp_path) -> None:
    store, _environment, _agent, runtime, job, _binding = make_coding_job(tmp_path)
    store.set_metadata_values(
        job.job_name,
        {"afk_stage": "implementation", "afk_outcome": "waiting for primary agent"},
    )
    cleanup = store.begin_job_end(job.job_name, interrupt=True)
    store.finish_job_end(job.job_name, cleanup.id, interrupted=True)

    pulse = build_coding_job_pulse(store, runtime.inspect(job.job_name))

    assert pulse.outcome_stage == "ended"
    assert pulse.lifecycle.state == "ended"
    assert pulse.lifecycle.source == "runtime"
    assert pulse.lifecycle.provenance == "fact"
    assert pulse.latest_delta.summary == "Job lifecycle is ended"
    assert pulse.latest_delta.source == "runtime"
    assert pulse.latest_delta.updated_at == pulse.lifecycle.updated_at
    assert pulse.observed_activity.status == "settled"
    assert pulse.attention.state == "none"


def test_verify_bounds_repeated_gate_failures_as_needs_human(tmp_path) -> None:
    store, environment, _agent, runtime, job, _binding = make_coding_job(tmp_path)
    initial = store.list_job_inputs(job.job_name)[0]
    assert runtime.deliver_input(job.job_name, initial.id).status == "succeeded"

    with pytest.raises(WorkflowFailure) as raised:
        workflow(
            store,
            environment,
            runtime,
            job,
            GitHubClient(),
            contract=RepoContract(
                mode="configured",
                commands={"check": "false"},
                env={},
                review=ReviewConfig(
                    max_rounds=1,
                    agents={
                        "droid": ReviewAgent(name="droid", command="reviewer {dorf_review_prompt}")
                    },
                ),
            ),
        ).verify()

    assert raised.value.kind == "needs-human"
    assert store.get_job("checkout-perf").status == "open"
