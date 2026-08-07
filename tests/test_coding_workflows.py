import json
import sqlite3
import subprocess
from pathlib import Path

import pytest

from dorf.runtime import (
    AgentConversationInspection,
    ArtifactInput,
    JobRuntime,
    NewJob,
    NewWorker,
    WorkerRuntime,
    WorkerTurnOutcome,
)
from dorf.workflows import (
    CodingStore,
    CodingWorkflow,
    WorkflowFailure,
    build_coding_job_pulse,
)
from dorf.workflows.coding import CommandInterrupted


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


class JobExecution:
    """Test collaborator for policy retained beyond the deleted Python terminal."""

    def __init__(self, binding, runtime, environment, agent, token):
        self.binding = binding
        self.runtime = runtime
        self.environment = environment

    def admit_message(self, **kwargs):
        return self.runtime.admit_message(self.binding.job.name, **kwargs)

    def deliver_input(self, input_id):
        return self.runtime.deliver_input(self.binding.job.name, input_id)

    def execute(self, argv, **kwargs):
        return self.environment.execute(self.binding, argv, **kwargs)

    def process_command(self, argv, **kwargs):
        return self.environment.process_command(self.binding, argv, **kwargs)

    def refresh_git_credentials(self):
        self.environment.refresh_git_credentials()


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
    reviewer=None,
):
    binding = store.get_job_binding(job.job_name)
    assert binding is not None
    return CodingWorkflow(
        store=store,
        job=job,
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

    outcome = workflow(store, environment, runtime, job, github).verify()

    assert outcome.messages[-1].text == "Verify passed for checkout-perf"
    assert store.get_coding_job("checkout-perf").status == "ready"
    assert store.get_coding_job("checkout-perf").github_pr_number == 42
    assert store.get_job("checkout-perf").status == "open"
    assert store.get_worker("coder-checkout-perf").status == "assigned"
    assert not any("codex exec" in item[1][-1] for item in environment.processes)


def test_verify_reports_unrecovered_job_turn_without_accessing_session_fields(tmp_path) -> None:
    store, environment, _agent, runtime, job, _binding = make_coding_job(tmp_path)
    add_recovery_required_job_turn(store, runtime, job.job_name)

    with pytest.raises(WorkflowFailure) as raised:
        workflow(store, environment, runtime, job, GitHubClient()).verify()

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


def test_review_finding_repairs_through_same_job_fifo_then_rereviews(tmp_path) -> None:
    finding = "- [P1] Cover the regression"
    clean = "DORF_REVIEW_NO_FINDINGS"
    store, environment, agent, runtime, job, _binding = make_coding_job(tmp_path)
    initial = store.list_job_inputs(job.job_name)[0]
    assert runtime.deliver_input(job.job_name, initial.id).status == "succeeded"
    github = GitHubClient()
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
        reviewer=reviewer,
    ).verify()

    inputs = store.list_job_inputs(job.job_name)
    assert [item.sequence for item in inputs] == [1, 2]
    assert inputs[1].kind == "message"
    assert "DeepSeek diff advisory findings" in inputs[1].text
    assert len(agent.turns) == 2
    assert reviewer.commits == ["b" * 40, "c" * 40]
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


def test_mismatched_retained_verifier_run_is_retried_at_the_exact_commit(tmp_path) -> None:
    store, environment, _agent, runtime, job, _binding = make_coding_job(tmp_path)
    stale = DiffReviewer(store)
    run = stale(job, "b" * 40)
    store.set_command_run_git_commits(run.id, before="b" * 40, after="d" * 40)
    reviewer = DiffReviewer(store)

    workflow(store, environment, runtime, job, GitHubClient(), reviewer=reviewer).verify()

    assert reviewer.commits == ["b" * 40]


def test_verify_retries_abandoned_but_not_concurrent_deepseek_run(tmp_path) -> None:
    store, environment, _agent, runtime, job, _binding = make_coding_job(tmp_path)
    running = store.create_command_run(
        job.job_name, "verify-role:diff", "pi deepseek-v4-flash", ""
    )
    store.set_command_run_git_commits(running.id, before="b" * 40, after=None)
    reviewer = DiffReviewer(store)

    with store.coding_verifier_lock(job.job_name) as acquired:
        assert acquired
        with pytest.raises(WorkflowFailure) as active:
            workflow(
                store,
                environment,
                runtime,
                job,
                GitHubClient(),
                reviewer=reviewer,
            ).verify()
    assert active.value.kind == "active-command"
    assert store.get_command_run(running.id).status == "running"

    current = store.get_coding_job(job.job_name)
    workflow(
        store,
        environment,
        runtime,
        current,
        GitHubClient(),
        reviewer=reviewer,
    ).verify()

    assert store.get_command_run(running.id).status == "interrupted"
    assert reviewer.commits == ["b" * 40]


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
    reviewer = DiffReviewer(store)

    outcome = workflow(
        store,
        environment,
        runtime,
        job,
        GitHubClient(),
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
