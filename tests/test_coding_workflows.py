import sqlite3
import subprocess
from pathlib import Path

import pytest

from dorf.cli import run_repository_preparation_or_raise
from dorf.command_runner import shell_command
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
from dorf.workflows import (
    CodingStore,
    CodingWorkflow,
    WorkflowFailure,
    build_coding_job_pulse,
    run_coding_job_command,
)
from dorf.workflows.coding import verify_job_readiness
from dorf.workflows.coding_commands import prepare_coding_repository


class Agent:
    agent_type = "codex"

    def __init__(self) -> None:
        self.turns = []

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

    def execute(self, binding, argv, **kwargs):
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

    def process_command(self, binding, argv, **kwargs):
        self.processes.append((binding, argv, kwargs))
        if argv[-1].startswith(("reviewer ", "codex exec ")):
            output = next(self.review_outputs)
            return ["bash", "-lc", f"printf '%s' {output!r}"]
        if argv[-1] == "false":
            return ["bash", "-lc", "exit 1"]
        return ["bash", "-lc", "printf 'command output\\n'"]


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

    def list_pull_requests_for_branch(self, *args, **kwargs):
        return []

    def create_pull_request(self, repo, **payload):
        self.created.append((repo, payload))
        return {"number": 42, "html_url": "https://github.test/pull/42"}

    def update_pull_request(self, repo, number, **payload):
        return {"number": number, "html_url": "https://github.test/pull/42"}

    def mark_pull_request_ready(self, repo, number):
        pass

    def add_pull_request_comment(self, repo, number, body):
        self.comments.append(body)

    def get_branch_sha(self, repo, branch):
        return "b" * 40

    def get_pull_request(self, repo, number):
        return {"number": number, "state": "open", "head": {"sha": "b" * 40}}

    def graphql(self, query, variables):
        return {"data": {"repository": {"pullRequest": {"reviewThreads": {"nodes": []}}}}}

    def list_pull_request_comments(self, repo, number):
        return []


def workflow(
    store,
    environment,
    runtime,
    job,
    github,
    *,
    contract=None,
):
    return CodingWorkflow(
        store=store,
        job=job,
        contract=contract or RepoContract(mode="generic", commands={}, env={}),
        environment=environment,
        runtime=runtime,
        github_client=lambda: github,
        refresh_git_credentials=lambda binding: None,
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


def test_coding_command_runs_in_assignment_workspace_and_records_fact_evidence(
    tmp_path,
) -> None:
    store, environment, _agent, _runtime, job, binding = make_coding_job(tmp_path)

    run = run_coding_job_command(
        store,
        environment,
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

    def execute(actual_binding, argv, **kwargs):
        nonlocal pushes
        assert actual_binding == binding
        if argv[:2] == ["git", "push"]:
            pushes += 1
            if pushes == 1:
                return subprocess.CompletedProcess(argv, 1, "", "Authentication failed")
        return original_execute(actual_binding, argv, **kwargs)

    environment.execute = execute
    refreshed = []

    class BehindGitHub(GitHubClient):
        def get_branch_sha(self, repo, branch):
            return "c" * 40

    readiness = verify_job_readiness(
        store,
        environment,
        job,
        RepoContract(mode="generic", commands={}, env={}),
        github_client=lambda: BehindGitHub(),
        refresh_git_credentials=lambda actual: refreshed.append(actual),
    )

    assert readiness.failures == []
    assert refreshed == [binding]
    assert pushes == 2


def test_review_harness_controls_provider_route_capability_not_agent_name(tmp_path) -> None:
    store, environment, _agent, runtime, job, binding = make_coding_job(
        tmp_path,
        review_outputs=["first", "second"],
    )
    contract = RepoContract(
        mode="configured",
        commands={},
        env={},
        review=ReviewConfig(
            agents={
                "secondary": ReviewAgent("secondary", "codex exec {dorf_review_prompt}"),
                "codex": ReviewAgent("codex", "reviewer {dorf_review_prompt}"),
            }
        ),
    )

    workflow(store, environment, runtime, job, GitHubClient(), contract=contract).review()

    assert len(environment.processes) == 2
    assert all(item[0] == binding for item in environment.processes)
    assert all(item[2]["cwd"] == binding.workspace for item in environment.processes)
    assert environment.processes[0][2]["provider_route"] is True
    assert environment.processes[1][2]["provider_route"] is False


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


def test_command_capability_not_command_kind_controls_provider_route(tmp_path) -> None:
    store, environment, _agent, _runtime, job, binding = make_coding_job(tmp_path)

    run_coding_job_command(
        store,
        environment,
        job,
        binding,
        RepoContract(mode="configured", commands={}, env={}),
        shell_command(
            "review:codex",
            "reviewer",
            requires_provider_route=False,
        ),
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
    clean = "DORF_REVIEW_NO_FINDINGS"
    store, environment, _agent, runtime, job, _binding = make_coding_job(
        tmp_path, review_outputs=[clean]
    )
    github = GitHubClient()
    contract = RepoContract(
        mode="configured",
        commands={},
        env={},
        review=ReviewConfig(
            max_rounds=1,
            agents={"droid": ReviewAgent(name="droid", command="reviewer {dorf_review_prompt}")},
        ),
    )

    outcome = workflow(store, environment, runtime, job, github, contract=contract).verify()

    assert outcome.messages[-1].text == "Verify passed for checkout-perf"
    assert store.get_coding_job("checkout-perf").status == "ready"
    assert store.get_coding_job("checkout-perf").github_pr_number == 42
    assert store.get_job("checkout-perf").status == "open"
    assert store.get_worker("coder-checkout-perf").status == "assigned"


def test_verify_reports_unrecovered_job_turn_without_accessing_session_fields(tmp_path) -> None:
    store, environment, _agent, runtime, job, _binding = make_coding_job(tmp_path)
    add_recovery_required_job_turn(store, runtime, job.job_name)
    contract = RepoContract(
        mode="configured",
        commands={},
        env={},
        review=ReviewConfig(
            max_rounds=1,
            agents={"droid": ReviewAgent("droid", "reviewer {dorf_review_prompt}")},
        ),
    )

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
    store, environment, agent, runtime, job, _binding = make_coding_job(
        tmp_path, review_outputs=[finding, clean]
    )
    initial = store.list_job_inputs(job.job_name)[0]
    assert runtime.deliver_input(job.job_name, initial.id).status == "succeeded"
    github = GitHubClient()
    contract = RepoContract(
        mode="configured",
        commands={},
        env={},
        review=ReviewConfig(
            max_rounds=2,
            agents={"droid": ReviewAgent(name="droid", command="reviewer {dorf_review_prompt}")},
        ),
    )

    workflow(store, environment, runtime, job, github, contract=contract).verify()

    inputs = store.list_job_inputs(job.job_name)
    assert [item.sequence for item in inputs] == [1, 2]
    assert inputs[1].kind == "message"
    assert "Verify payload" in inputs[1].text
    assert len(agent.turns) == 2
    assert store.get_job(job.job_name).status == "open"


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
    clean = "DORF_REVIEW_NO_FINDINGS"
    store, environment, _agent, runtime, job, binding = make_coding_job(
        tmp_path, review_outputs=[clean]
    )
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
            agents={"droid": ReviewAgent("droid", "reviewer {dorf_review_prompt}")},
        ),
    )

    outcome = workflow(
        store, environment, runtime, job, GitHubClient(), contract=contract
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
