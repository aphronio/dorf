"""Behavior tests for the bounded shadow/advisory verification role (issue #32)."""

import hashlib
import subprocess
import textwrap
from pathlib import Path

import pytest

from dorf.github_app import GitHubRepositoryError
from dorf.repo_contract import ContractValidationError, VerifierRole, load_repo_contract
from dorf.verifier_tooling import (
    NODE_SHA256,
    NODE_URL,
    NODE_VERSION,
    PI_BIN_PATH,
    PI_CLI_PATH,
    PI_EXTENSION_PATH,
    PI_PACKAGE_DIR,
    PI_PREFIX,
    PI_SHA512,
    PI_TARBALL,
    PI_URL,
    PI_VERSION,
    install_verifier_tooling_script,
    node_install_plan,
    pi_command,
    pi_install_plan,
    pi_provider_extension,
)
from dorf.workflows import (
    CodingJob,
    CodingStore,
    VerifierCoordinator,
    VerifierInfrastructureFailed,
    render_verifier_protocol,
    verifier_clone_script,
    verifier_config_digest,
    verifier_config_snapshot,
    verifier_worker_name,
)
from dorf.workflows.verifier import (
    REVIEW_NO_FINDINGS_SENTINEL,
    VERIFIER_FINDINGS_CHAR_LIMIT,
    _parse_verifier_verdict,
    verifier_findings_message,
)

COMMIT = "c" * 40
START = "a" * 40
TREE = "t" * 40
MODEL = "deepseek-v4-flash"


class FakeBinding:
    def __init__(self, name: str, *, status: str = "ready") -> None:
        self.worker = type("W", (), {"name": name})()
        self.room = type("R", (), {"id": f"room-{name}", "status": status})()


class FakeExecution:
    def __init__(self, binding, fake: "FakeDorf") -> None:
        self.binding = binding
        self.fake = fake
        self.processed = []
        self.written = {}

    def check_codex_authentication(self) -> None:
        self.fake.authentication_checks.append(self.binding.worker.name)

    def execute(self, argv, **kwargs):
        self.fake.executions.append((self.binding.worker.name, argv, kwargs))
        script = argv[-1] if argv[:2] == ["bash", "-lc"] else None
        if script is not None and "verifier clone ready" in script:
            return subprocess.CompletedProcess(argv, 0, "verifier clone ready\n", "")
        if script is not None and "verifier-tooling: ready" in script:
            return subprocess.CompletedProcess(argv, 0, "verifier-tooling: ready\n", "")
        if script is not None and "review-diff.txt" in script:
            return subprocess.CompletedProcess(argv, 0, "", "")
        if script is not None and "cat >" in script:
            path = script.rsplit("cat > ", 1)[1].split()[0]
            self.written[path] = kwargs.get("input", "")
            return subprocess.CompletedProcess(argv, 0, "", "")
        if script is not None and "rm -rf" in script:
            return subprocess.CompletedProcess(argv, 0, "", "")
        if argv[:2] == ["git", "rev-parse"] and argv[-1] == "HEAD":
            head = self.fake.head_after if self.fake.pi_ran else self.fake.head
            return subprocess.CompletedProcess(argv, 0, f"{head}\n", "")
        if argv[:2] == ["git", "rev-parse"] and argv[-1] == "HEAD^{tree}":
            return subprocess.CompletedProcess(argv, 0, f"{TREE}\n", "")
        if argv[:2] == ["git", "status"]:
            status = self.fake.worktree_after if self.fake.pi_ran else self.fake.worktree
            return subprocess.CompletedProcess(argv, 0, status, "")
        if argv[:2] == ["git", "-C"]:
            return subprocess.CompletedProcess(argv, 0, "", "")
        raise AssertionError(f"unexpected verifier command: {argv!r} {kwargs!r}")

    def process_command(self, argv, **kwargs):
        self.processed.append((argv, kwargs))
        self.fake.pi_ran = True
        if self.fake.review_exit_code:
            return [
                "bash",
                "-lc",
                f"printf '%b' {self.fake.review_output!r}; exit {self.fake.review_exit_code}",
            ]
        return ["bash", "-lc", f"printf '%b' {self.fake.review_output!r}"]


class FakeDorf:
    def __init__(self) -> None:
        self.bindings: dict[str, FakeBinding] = {}
        self.spawns = []
        self.ended = []
        self.messages = []
        self._message_ids = {}
        self.authentication_checks = []
        self.executions = []
        self.worker_executions: dict[str, FakeExecution] = {}
        self.head = COMMIT
        self.head_after = COMMIT
        self.worktree = ""
        self.worktree_after = ""
        self.pi_ran = False
        self.review_output = ""
        self.review_exit_code = 0

    def get_worker_binding(self, name):
        return self.bindings.get(name)

    def spawn_worker(self, name, **kwargs):
        self.spawns.append((name, kwargs))
        self.bindings[name] = FakeBinding(name)
        self.pi_ran = False  # a fresh Room starts with a fresh observation cycle
        return self.bindings[name]

    def end_worker(self, name, *, interrupt=False):
        self.ended.append((name, interrupt))
        self.bindings[name] = FakeBinding(name, status="absent")

    def worker_execution(self, name):
        execution = self.worker_executions.get(name)
        if execution is None:
            execution = FakeExecution(self.bindings[name], self)
            self.worker_executions[name] = execution
        return execution

    def message_job(self, name, text, *, action_id=None):
        if action_id in self._message_ids:
            input_id = self._message_ids[action_id]
            created = False
        else:
            input_id = f"jmsg-{len(self.messages) + 1}"
            self._message_ids[action_id] = input_id
            created = True
        self.messages.append((name, text, action_id, input_id, created))
        receipt = type("R", (), {"job_input": type("I", (), {"id": input_id})()})()
        return receipt


class FakeGateway:
    def __init__(self) -> None:
        self.revoked = []
        self.revoke_failures = 0
        self.revoke_result = True

    def route_for_consumer(self, consumer):
        route = type(
            "Route",
            (),
            {
                "id": "route-1",
                "base_url": "http://127.0.0.1:8317/v1",
                "model_prefix": "deepseek",
            },
        )
        return route

    def revoke_route(self, route_id):
        if self.revoke_failures > 0:
            self.revoke_failures -= 1
            raise RuntimeError("broker refused revocation")
        self.revoked.append(route_id)
        return self.revoke_result


class FakeGitHub:
    def __init__(self, sha: str = COMMIT) -> None:
        self.sha = sha

    def get_branch_sha(self, repo_full_name, branch):
        return self.sha


def make_job_and_store(tmp_path: Path) -> tuple[CodingStore, CodingJob]:
    store = CodingStore.open(tmp_path / "state.sqlite3")
    job = store.create_coding_job(
        job_name="checkout-perf",
        metadata={
            "task": "Improve checkout performance",
            "target_repo": str(tmp_path / "repo"),
            "target_branch": "main",
            "target_start_sha": START,
            "job_branch": "dorf/checkout-perf",
            "github_repo": "example/repo",
        },
    )
    (tmp_path / "repo").mkdir()
    store.documents.jobs.path(job.job_name).mkdir(parents=True, exist_ok=True)
    return store, job


def make_role(
    *,
    authority: str = "advisory",
    connection: str = "deepseek-review",
    model: str = MODEL,
    reasoning_effort: str = "max",
    timeout_seconds: int = 120,
    prompt: str = "Review the exact implementation diff with a strict rubric.",
) -> VerifierRole:
    return VerifierRole(
        name="diff",
        harness="pi",
        connection=connection,
        model=model,
        reasoning_effort=reasoning_effort,
        authority=authority,
        room="dedicated",
        timeout_seconds=timeout_seconds,
        prompt=prompt,
    )


def make_coordinator(
    tmp_path: Path,
    *,
    dorf: FakeDorf | None = None,
    gateway: FakeGateway | None = None,
    github: FakeGitHub | None = None,
    review_output: str = "",
    authority: str = "advisory",
):
    store, job = make_job_and_store(tmp_path)
    dorf = dorf or FakeDorf()
    dorf.review_output = review_output
    return (
        VerifierCoordinator(
            store=store,
            job=job,
            role=make_role(authority=authority),
            dorf=dorf,
            gateway=gateway or FakeGateway(),
            github_client=github or FakeGitHub(),
            token_provider=lambda: "installation-token",
        ),
        store,
        job,
        dorf,
    )


def make_run(
    store: CodingStore,
    *,
    job_name: str,
    role: VerifierRole,
    commit: str = COMMIT,
    generation: int = 1,
    worker_name: str | None = None,
    room_id: str = "",
    route_id: str = "",
):
    """Reserve one run row with the exact typed configuration of ``role``."""
    return store.create_verifier_run(
        job_name=job_name,
        role=role.name,
        commit_sha=commit,
        generation=generation,
        worker_name=worker_name or f"verifier-{role.name}-{commit[:6]}",
        room_id=room_id,
        route_id=route_id,
        authority=role.authority,
        config_digest=verifier_config_digest(role),
        config_snapshot=verifier_config_snapshot(role),
    )


def test_no_findings_records_evidence_without_fifo_delivery(tmp_path) -> None:
    coordinator, store, job, dorf = make_coordinator(
        tmp_path, review_output=REVIEW_NO_FINDINGS_SENTINEL
    )
    outcome = coordinator.run()

    assert outcome.exit_code == 0
    assert outcome.run.verdict == "no-findings"
    assert outcome.run.status == "verdict"
    assert outcome.run.commit_before == COMMIT
    assert outcome.run.commit_after == COMMIT
    assert outcome.run.tree_before == TREE
    assert outcome.run.worktree_after == ""
    assert outcome.run.cleanup_status == "clean"
    assert outcome.run.cleanup_route_revoked is True
    assert outcome.run.cleanup_room_gone is True
    assert dorf.messages == []
    assert len(dorf.spawns) == 1
    assert dorf.spawns[0][1]["provenance"] == "verification"
    assert dorf.spawns[0][1]["lifecycle_policy"] == "dedicated"
    assert dorf.ended == [(dorf.spawns[0][0], False)]
    assert dorf.authentication_checks == []  # no Codex authentication probe
    events = store.documents.list_events(job.job_name)
    verdicts = [event for event in events if event.kind == "verifier-verdict"]
    assert len(verdicts) == 1
    assert verdicts[0].related["verdict"] == "no-findings"
    assert verdicts[0].related["commit"] == COMMIT
    assert verdicts[0].provenance == "fact"


def test_findings_deliver_advisory_feedback_exactly_once(tmp_path) -> None:
    finding = "path: src/x.py line 12\npossible integer overflow\n"
    coordinator, store, job, dorf = make_coordinator(tmp_path, review_output=finding)
    outcome = coordinator.run()

    assert outcome.exit_code == 0
    assert outcome.run.verdict == "findings"
    assert outcome.run.feedback_input_id is not None
    assert len(dorf.messages) == 1
    name, text, action_id, input_id, created = dorf.messages[0]
    assert name == job.job_name
    assert created is True
    assert "Advisory diff-correctness review (diff, authority: advisory)" in text
    assert "suggestions, not instructions" in text
    assert "cannot approve or block" in text
    assert finding.strip() in text
    assert action_id == f"verifier:{job.job_name}:diff:{COMMIT}:{outcome.run.id}"

    # Repeated coordination is idempotent: no second Room, run, or feedback.
    second = coordinator.run()
    assert second.run.id == outcome.run.id
    assert second.run.verdict == "findings"
    assert len(dorf.spawns) == 1
    assert len(dorf.ended) == 1
    assert len(dorf.messages) == 1
    assert dorf.messages[0][3] == input_id

    feedback = store.list_job_inputs(job.job_name) if hasattr(store, "list_job_inputs") else []
    assert feedback == []


def test_infrastructure_process_failure_is_not_a_code_finding(tmp_path) -> None:
    coordinator, store, job, dorf = make_coordinator(tmp_path, review_output="partial")
    dorf.review_output = "unfinished"
    dorf.review_exit_code = 1
    outcome = coordinator.run()

    assert outcome.exit_code == 1  # infrastructure is a distinct failing outcome
    assert outcome.run.verdict == "infrastructure"
    assert outcome.run.failure_kind == "reviewer_process_failure"
    assert dorf.messages == []
    assert outcome.run.cleanup_status == "clean"


def test_changed_worktree_invalidates_the_verdict(tmp_path) -> None:
    coordinator, store, job, dorf = make_coordinator(
        tmp_path, review_output=REVIEW_NO_FINDINGS_SENTINEL
    )
    dorf.worktree_after = "?? stray.txt\n"
    outcome = coordinator.run()

    assert outcome.run.verdict == "infrastructure"
    assert outcome.run.failure_kind == "commit_changed"
    assert dorf.messages == []
    assert outcome.run.cleanup_status == "clean"


def test_changed_commit_invalidates_the_verdict(tmp_path) -> None:
    coordinator, _, _, dorf = make_coordinator(
        tmp_path, review_output=REVIEW_NO_FINDINGS_SENTINEL
    )
    dorf.head_after = "d" * 40
    outcome = coordinator.run()

    assert outcome.run.verdict == "infrastructure"
    assert outcome.run.failure_kind == "commit_changed"


def test_timeout_is_an_infrastructure_outcome(tmp_path) -> None:
    store, job = make_job_and_store(tmp_path)
    dorf = FakeDorf()
    dorf.review_output = "slow"

    class SlowExecution(FakeExecution):
        def process_command(self, argv, **kwargs):
            self.processed.append((argv, kwargs))
            return ["bash", "-lc", "sleep 5"]

    dorf.worker_execution = lambda name: SlowExecution(dorf.bindings[name], dorf)
    role = VerifierRole(
        name="diff",
        harness="pi",
        connection="deepseek-review",
        model=MODEL,
        reasoning_effort="max",
        authority="advisory",
        room="dedicated",
        timeout_seconds=1,
        prompt="p",
    )
    coordinator = VerifierCoordinator(
        store=store,
        job=job,
        role=role,
        dorf=dorf,
        gateway=FakeGateway(),
        github_client=FakeGitHub(),
        token_provider=lambda: "token",
    )
    outcome = coordinator.run()

    assert outcome.run.verdict == "infrastructure"
    assert outcome.run.failure_kind == "timed_out"
    assert dorf.messages == []
    assert outcome.run.cleanup_status == "clean"


def test_cleanup_is_retry_safe_and_visibly_pending_until_reconciled(tmp_path) -> None:
    coordinator, store, _, dorf = make_coordinator(
        tmp_path, review_output=REVIEW_NO_FINDINGS_SENTINEL
    )
    coordinator._gateway.revoke_failures = 1  # type: ignore[attr-defined]
    outcome = coordinator.run()

    assert outcome.exit_code == 1
    assert "cleanup is incomplete" in outcome.messages[-1]
    run = store.get_verifier_run(outcome.run.id)
    assert run.cleanup_status == "pending"
    assert run.cleanup_route_revoked is False
    assert run.cleanup_room_gone is True

    second = coordinator.run()
    assert second.run.id == outcome.run.id
    assert second.exit_code == 0
    assert second.run.cleanup_status == "clean"
    assert second.run.cleanup_route_revoked is True
    assert len(dorf.spawns) == 1  # no second Room


def test_revoke_route_false_counts_as_already_absent(tmp_path) -> None:
    coordinator, store, _, dorf = make_coordinator(
        tmp_path, review_output=REVIEW_NO_FINDINGS_SENTINEL
    )
    coordinator._gateway.revoke_result = False  # type: ignore[attr-defined]
    outcome = coordinator.run()

    assert outcome.exit_code == 0
    assert outcome.run.cleanup_status == "clean"
    assert outcome.run.cleanup_route_revoked is True
    assert coordinator._gateway.revoked == ["route-1"]  # type: ignore[attr-defined]


def test_never_provisioned_identities_clean_without_gateway_or_dorf(tmp_path) -> None:
    coordinator, store, job, dorf = make_coordinator(tmp_path)
    gateway = coordinator._gateway
    run = make_run(
        store,
        job_name=job.job_name,
        role=make_role(),
        worker_name="verifier-never-provisioned",
    )
    store.finish_verifier_run(run.id, verdict="no-findings")

    outcome = coordinator.run()

    assert outcome.exit_code == 0
    assert outcome.run.id == run.id
    assert outcome.run.cleanup_status == "clean"
    assert outcome.run.cleanup_route_revoked is True
    assert outcome.run.cleanup_room_gone is True
    assert gateway.revoked == []
    assert dorf.ended == []


def test_resume_after_crash_before_room_record_reuses_existing_worker(
    tmp_path,
) -> None:
    coordinator, store, _, dorf = make_coordinator(
        tmp_path, review_output=REVIEW_NO_FINDINGS_SENTINEL
    )

    worker_name = verifier_worker_name(
        "checkout-perf",
        "diff",
        COMMIT,
        1,
        config_digest=verifier_config_digest(make_role()),
    )
    # The crashed attempt spawned the deterministic Worker, then died before
    # recording the Room identity in the running run.
    first = make_run(
        store,
        job_name="checkout-perf",
        role=make_role(),
        worker_name=worker_name,
    )
    dorf.bindings[worker_name] = FakeBinding(worker_name)

    outcome = coordinator.run()

    assert outcome.run.id == first.id
    assert outcome.run.generation == 1
    assert outcome.run.verdict == "no-findings"
    assert len(dorf.spawns) == 0  # recovered, not duplicated
    assert outcome.run.room_id == f"room-{worker_name}"
    assert outcome.run.cleanup_status == "clean"
    assert dorf.ended == [(worker_name, False)]


def test_verdict_event_is_ensured_during_terminal_reconciliation(tmp_path) -> None:
    coordinator, store, job, dorf = make_coordinator(tmp_path)
    # Crash window: the verdict was stored but the event was never appended.
    run = make_run(
        store,
        job_name=job.job_name,
        role=make_role(),
        worker_name="verifier-crashed-after-verdict",
        room_id="room-x",
        route_id="route-x",
    )
    store.finish_verifier_run(run.id, verdict="no-findings")
    assert store.documents.list_events(job.job_name) == []

    outcome = coordinator.run()

    assert outcome.run.id == run.id
    assert outcome.exit_code == 0
    verdicts = [
        event
        for event in store.documents.list_events(job.job_name)
        if event.kind == "verifier-verdict"
    ]
    assert len(verdicts) == 1
    assert verdicts[0].related["verdict"] == "no-findings"
    assert verdicts[0].related["input"] == ""

    # Reconciliation is idempotent: no duplicate event on the next invocation.
    coordinator.run()
    verdicts = [
        event
        for event in store.documents.list_events(job.job_name)
        if event.kind == "verifier-verdict"
    ]
    assert len(verdicts) == 1


def test_findings_event_links_feedback_truthfully_after_delivery(tmp_path) -> None:
    coordinator, store, job, dorf = make_coordinator(
        tmp_path, review_output="finding: off by one"
    )
    run = make_run(
        store,
        job_name=job.job_name,
        role=make_role(),
        worker_name="verifier-findings-crash",
        room_id="room-x",
        route_id="route-x",
    )
    store.finish_verifier_run(run.id, verdict="findings")

    outcome = coordinator.run()

    assert outcome.run.id == run.id
    assert outcome.run.feedback_input_id is not None
    assert len(dorf.messages) == 1
    verdicts = [
        event
        for event in store.documents.list_events(job.job_name)
        if event.kind == "verifier-verdict"
    ]
    assert len(verdicts) == 1
    assert verdicts[0].related["input"] == outcome.run.feedback_input_id


def test_resume_after_crash_reuses_the_room_without_duplicating_the_run(
    tmp_path,
) -> None:
    coordinator, store, _, dorf = make_coordinator(
        tmp_path, review_output=REVIEW_NO_FINDINGS_SENTINEL
    )
    # A crashed prior attempt left a running run with its Room alive.
    first = make_run(
        store,
        job_name="checkout-perf",
        role=make_role(),
        worker_name="verifier-crashed",
    )
    store.set_verifier_run_room(first.id, "room-verifier-crashed", "route-1")
    dorf.bindings["verifier-crashed"] = FakeBinding("verifier-crashed")

    outcome = coordinator.run()

    assert outcome.run.id == first.id
    assert outcome.run.generation == 1
    assert outcome.run.verdict == "no-findings"
    assert len(dorf.spawns) == 0  # the surviving Room is reused, not duplicated
    assert outcome.run.cleanup_status == "clean"
    assert dorf.ended == [("verifier-crashed", False)]


def test_lost_room_marks_infrastructure_and_retry_uses_a_new_generation(
    tmp_path,
) -> None:
    coordinator, store, _, dorf = make_coordinator(
        tmp_path, review_output=REVIEW_NO_FINDINGS_SENTINEL
    )

    def lose_room(name):
        dorf.bindings[name] = FakeBinding(name, status="absent")

    dorf.end_worker = lambda name, interrupt=False: lose_room(name)
    outcome = coordinator.run()
    assert outcome.run.verdict == "no-findings"

    # Simulate a crashed run whose Room was lost before a verdict.
    make_run(
        store,
        job_name="checkout-perf",
        role=make_role(),
        commit="e" * 40,
        worker_name="verifier-lost",
        room_id="room-lost",
        route_id="route-lost",
    )
    dorf.bindings["verifier-lost"] = FakeBinding("verifier-lost", status="absent")
    coordinator._github.sha = "e" * 40  # type: ignore[attr-defined]
    dorf.head = "e" * 40
    dorf.head_after = "e" * 40
    with pytest.raises(VerifierInfrastructureFailed) as caught:
        coordinator.run()
    assert caught.value.failure_kind == "room_lost"
    lost = store.get_latest_verifier_run(
        job_name="checkout-perf", role="diff", commit_sha="e" * 40
    )
    assert lost is not None
    assert lost.status == "infrastructure"
    assert lost.failure_kind == "room_lost"

    # A retry for the same commit starts a fresh generation.
    dorf.bindings.pop("verifier-lost", None)
    retry = coordinator.run()
    assert retry.run.generation == 2
    assert retry.run.verdict == "no-findings"
    assert retry.run.cleanup_status == "clean"


def test_branch_resolution_failure_is_infrastructure(tmp_path) -> None:
    coordinator, store, job, dorf = make_coordinator(tmp_path)

    class MissingBranch(FakeGitHub):
        def get_branch_sha(self, repo_full_name, branch):
            raise GitHubRepositoryError("branch not found")

    coordinator._github = MissingBranch()
    with pytest.raises(VerifierInfrastructureFailed) as caught:
        coordinator.run()
    assert caught.value.failure_kind == "branch_resolution"
    assert dorf.spawns == []
    assert store.list_verifier_runs(job.job_name) == []


def test_clone_script_pins_the_exact_commit_and_leaves_no_credentials(
    tmp_path,
    monkeypatch,
) -> None:
    origin = tmp_path / "origin"
    origin.mkdir()
    subprocess.run(["git", "init", "-q", "-b", "main"], cwd=origin, check=True)
    subprocess.run(["git", "config", "user.email", "t@t"], cwd=origin, check=True)
    subprocess.run(["git", "config", "user.name", "t"], cwd=origin, check=True)
    (origin / "file.txt").write_text("one\n")
    subprocess.run(["git", "add", "."], cwd=origin, check=True)
    subprocess.run(["git", "commit", "-qm", "base"], cwd=origin, check=True)
    base = subprocess.run(
        ["git", "rev-parse", "HEAD"], cwd=origin, check=True, capture_output=True, text=True
    ).stdout.strip()
    (origin / "file.txt").write_text("two\n")
    subprocess.run(["git", "add", "."], cwd=origin, check=True)
    subprocess.run(["git", "commit", "-qm", "impl"], cwd=origin, check=True)
    head = subprocess.run(
        ["git", "rev-parse", "HEAD"], cwd=origin, check=True, capture_output=True, text=True
    ).stdout.strip()
    subprocess.run(["git", "branch", "dorf/job", "HEAD"], cwd=origin, check=True)
    subprocess.run(["git", "checkout", "-q", "main"], cwd=origin, check=True)

    workspace = tmp_path / "review"
    workspace.mkdir()
    script = verifier_clone_script(str(origin), "dorf/job", head, str(workspace))
    result = subprocess.run(
        ["bash", "-lc", script],
        input="x-access-token:super-secret\n",
        capture_output=True,
        text=True,
        cwd=tmp_path,
    )
    assert result.returncode == 0, result.stderr

    observed = subprocess.run(
        ["git", "rev-parse", "HEAD"], cwd=workspace, capture_output=True, text=True
    ).stdout.strip()
    assert observed == head
    status = subprocess.run(
        ["git", "status", "--porcelain"], cwd=workspace, capture_output=True, text=True
    ).stdout
    assert status == ""
    helper = subprocess.run(
        ["git", "config", "--local", "--get", "credential.helper"],
        cwd=workspace,
        capture_output=True,
        text=True,
    )
    assert helper.returncode != 0
    assert not (Path.home() / ".dorf-git-credentials").exists()
    remote = subprocess.run(
        ["git", "remote", "get-url", "origin"], cwd=workspace, capture_output=True, text=True
    ).stdout.strip()
    assert "super-secret" not in remote
    assert "x-access-token" not in remote
    assert base != head


def test_contract_role_rejects_repository_owned_commands(tmp_path) -> None:
    (tmp_path / ".dorf.toml").write_text(
        textwrap.dedent(
            """
            [verification.roles.diff]
            harness = "pi"
            connection = "deepseek-review"
            model = "deepseek-v4-flash"
            reasoning_effort = "max"
            authority = "advisory"
            room = "dedicated"
            command = "rm -rf /"
            """
        )
    )
    with pytest.raises(ContractValidationError, match="must not declare"):
        load_repo_contract(tmp_path)


def test_contract_role_rejects_codex_only_reasoning_level(tmp_path) -> None:
    (tmp_path / ".dorf.toml").write_text(
        textwrap.dedent(
            """
            [verification.roles.diff]
            harness = "pi"
            connection = "deepseek-review"
            model = "deepseek-v4-flash"
            reasoning_effort = "ultra"
            authority = "advisory"
            room = "dedicated"
            """
        )
    )
    with pytest.raises(ContractValidationError, match="reasoning_effort must be one of"):
        load_repo_contract(tmp_path)


def test_protocol_and_pi_command_pin_read_only_tools_and_prefixed_model() -> None:
    extension = pi_provider_extension(
        model=MODEL,
        prefix="deepseek",
    )
    assert "'deepseek/deepseek-v4-flash'" in extension
    assert "${DORF_PROVIDER_ROUTE_KEY}" in extension
    assert "openai-responses" in extension
    assert "max: \"max\"" in extension.replace("'", '"') or '"max": "max"' in extension

    argv = pi_command(
        role_name="diff",
        model=MODEL,
        prefix="deepseek",
        reasoning_effort="max",
        run_id=3,
    )
    command = argv[2]
    assert PI_CLI_PATH in command
    assert PI_EXTENSION_PATH in command
    assert "--tools read,grep,find,ls" in command
    assert "--no-session" in command
    assert "--no-approve" in command
    assert "--no-context-files" in command
    assert "--thinking max" in command
    assert "--model dorf-deepseek/deepseek/deepseek-v4-flash" in command
    assert "DORF_PROVIDER_ROUTE_KEY" not in command

    protocol = render_verifier_protocol(
        job_name="checkout-perf",
        job_branch="dorf/checkout-perf",
        target_branch="main",
        target_start_sha=START,
        commit=COMMIT,
        role_prompt="Review with a strict rubric.",
    )
    assert COMMIT in protocol
    assert "read, grep, find, ls" in protocol
    assert "never modify files" in protocol.casefold()
    assert REVIEW_NO_FINDINGS_SENTINEL in protocol
    assert "advisory" in protocol


def test_tooling_plan_is_version_and_integrity_pinned_without_credentials() -> None:
    node = node_install_plan()
    pi = pi_install_plan()
    assert node.url == NODE_URL
    assert NODE_VERSION in node.url
    assert node.expected_digest == NODE_SHA256
    assert pi.url == PI_URL
    assert PI_VERSION in pi.url
    assert pi.expected_digest == PI_SHA512
    script = install_verifier_tooling_script()
    assert NODE_SHA256 in script
    assert PI_SHA512 in script
    assert "api_key" not in script
    assert "DEEPSEEK" not in script
    assert "Bearer" not in script
    # The pinned Node must satisfy Pi's declared engine requirement (>=22.19.0).
    from dorf.verifier_tooling import NODE_VERSION as _NODE_VERSION

    parts = tuple(int(part) for part in _NODE_VERSION.split("."))
    assert len(parts) == 3 and parts >= (22, 19, 0)
    # Node stays a verified archive extraction: one top-level directory is
    # stripped and the sentinel check matches the touched plain file.
    assert "--strip-components=1" in script
    assert 'test -f "$target_dir/.installed"' in script
    # npm registry integrity is base64; the script must not compare it as hex.
    assert "openssl dgst -sha512 -binary" in script
    assert "base64 -w0" in script
    # Pi is installed through the pinned Node/npm directly from the verified
    # local tarball into the isolated global prefix -- never unpacked, and npm
    # never runs from inside the package.
    assert f'PATH="{PI_PREFIX}/node-v{NODE_VERSION}/bin:$PATH"' in script
    assert f'"{PI_PREFIX}/node-v{NODE_VERSION}/bin/npm" install --global' in script
    assert f"pi_prefix={PI_PREFIX}" in script
    assert '--prefix "$pi_prefix"' in script
    assert "--omit=dev --no-audit --no-fund" in script
    assert "$pi_staging" in script
    assert ".pi-coding-agent-0.83.0.tgz.dir" not in script  # no pi extraction dir
    # Idempotence checks the actual npm global layout.
    assert "test -x \"$pi_bin\" && test -f \"$pi_package_dir/dist/cli.js\"" in script
    # The expected on-disk layout is the npm global prefix layout.
    assert PI_PREFIX == "/opt/dorf/verifier"
    assert PI_BIN_PATH == f"{PI_PREFIX}/bin/pi"
    assert PI_PACKAGE_DIR == (
        f"{PI_PREFIX}/lib/node_modules/@earendil-works/pi-coding-agent"
    )
    assert PI_CLI_PATH == f"{PI_PACKAGE_DIR}/dist/cli.js"
    assert PI_EXTENSION_PATH == f"{PI_PREFIX}/extensions/dorf-deepseek-provider.mjs"
    assert "contextWindow: 1000000" in pi_provider_extension()
    assert "maxTokens: 384000" in pi_provider_extension()


FAKE_NPM = r"""#!/bin/sh
# Emulates the pinned npm global install for the verified Pi tarball and fails
# hard when the generated command does not use the exact verified shape: pinned
# Node via PATH, --global, isolated --prefix, the verified local tarball, the
# shrinkwrap-friendly flag set, and the isolated cache directory.
node_bin=$(cd "$(dirname "$0")" && pwd)
fail() {
  echo "fake npm rejected command: $1" >&2
  exit 9
}
[ "$#" -eq 10 ] || fail "expected exactly 10 arguments, got: $*"
[ "$1" = "install" ] || fail "argv1=$1 is not install"
[ "$2" = "--global" ] || fail "argv2=$2 is not --global"
[ "$3" = "--prefix" ] || fail "argv3=$3 is not --prefix"
[ "$6" = "--omit=dev" ] || fail "argv6=$6 is not --omit=dev"
[ "$7" = "--no-audit" ] || fail "argv7=$7 is not --no-audit"
[ "$8" = "--no-fund" ] || fail "argv8=$8 is not --no-fund"
[ "$9" = "--cache" ] || fail "argv9=$9 is not --cache"
case "${10}" in
  *"/.npm-cache") ;;
  *) fail "argv10=${10} is not the isolated npm cache" ;;
esac
case "$PATH" in
  "$node_bin":*) ;;
  *) fail "pinned node bin is not first on PATH: $PATH" ;;
esac
prefix="$4"
tarball="$5"
[ -f "$tarball" ] || fail "install source is not a file: $tarball"
case "$tarball" in
  *".pi-coding-agent-0.83.0.tgz") ;;
  *) fail "install source is not the verified local pi tarball: $tarball" ;;
esac
pkg="$prefix/lib/node_modules/@earendil-works/pi-coding-agent"
if [ -n "${FAKE_NPM_FAIL:-}" ]; then
  mkdir -p "$pkg/dist" "$prefix/bin"
  printf 'partial\n' > "$pkg/dist/cli.js"
  printf 'partial\n' > "$prefix/bin/pi"
  echo "fake npm failing as configured" >&2
  exit 1
fi
mkdir -p "$prefix"
echo call >> "$prefix/.fake-npm-calls"
mkdir -p "$pkg/dist" "$prefix/bin"
printf '%s\n' '#!/usr/bin/env node' "console.log('fake pi')" > "$pkg/dist/cli.js"
chmod 755 "$pkg/dist/cli.js"
cat > "$prefix/bin/pi" <<'SHIM'
#!/bin/sh
exec node "$(dirname "$0")/../lib/node_modules/@earendil-works/pi-coding-agent/dist/cli.js" "$@"
SHIM
chmod 755 "$prefix/bin/pi"
echo "fake npm: global install ok"
"""


def _build_fixture_tarballs(tmp_path: Path) -> tuple[Path, Path]:
    """Build gz archives shaped like the real nodejs.org and npm tarballs.

    The node archive carries the fake pinned npm (which emulates the global
    install and rejects wrong command shapes) as ``bin/npm``.
    """
    from dorf.verifier_tooling import NODE_VERSION as pinned_node

    node_root = tmp_path / "payload-node"
    node_dir = node_root / f"node-v{pinned_node}-linux-x64"
    (node_dir / "bin").mkdir(parents=True)
    (node_dir / "bin" / "node").write_text("#!/bin/sh\necho fake-node\n")
    (node_dir / "bin" / "node").chmod(0o755)
    (node_dir / "bin" / "npm").write_text(FAKE_NPM)
    (node_dir / "bin" / "npm").chmod(0o755)
    node_tarball = tmp_path / "node-fixture.tgz"
    subprocess.run(
        ["tar", "-czf", str(node_tarball), "-C", str(node_root), node_dir.name],
        check=True,
    )

    pi_root = tmp_path / "payload-pi"
    pi_dir = pi_root / "package"
    (pi_dir / "dist").mkdir(parents=True)
    (pi_dir / "dist" / "cli.js").write_text("#!/usr/bin/env node\nconsole.log('fake pi')\n")
    (pi_dir / "dist" / "cli.js").chmod(0o755)
    (pi_dir / "package.json").write_text(
        '{"name":"pi-coding-agent","bin":{"pi":"dist/cli.js"}}'
    )
    (pi_dir / "npm-shrinkwrap.json").write_text('{"lockfileVersion":3}')
    pi_tarball = tmp_path / PI_TARBALL
    subprocess.run(
        ["tar", "-czf", str(pi_tarball), "-C", str(pi_root), pi_dir.name],
        check=True,
    )
    return node_tarball, pi_tarball


def _fixture_plans(
    tmp_path: Path,
    *,
    node_tarball: Path,
    pi_tarball: Path,
    node_dir: Path,
    pi_prefix: Path,
) -> tuple:
    """Correct-digest install plans over the fixture archives."""
    import base64
    import hashlib

    from dorf.verifier_tooling import ToolInstallPlan

    node_plan = ToolInstallPlan(
        label="Node.js",
        url=node_tarball.as_uri(),
        filename=node_tarball.name,
        digest_algorithm="sha256",
        expected_digest=hashlib.sha256(node_tarball.read_bytes()).hexdigest(),
        install_directory=str(node_dir),
    )
    pi_plan = ToolInstallPlan(
        label="Pi coding agent",
        url=pi_tarball.as_uri(),
        filename=pi_tarball.name,
        digest_algorithm="sha512",
        expected_digest=base64.b64encode(
            hashlib.sha512(pi_tarball.read_bytes()).digest()
        ).decode(),
        install_directory=str(pi_prefix),
    )
    return node_plan, pi_plan


def test_tooling_install_script_uses_pinned_npm_and_global_prefix_layout(
    tmp_path,
) -> None:
    """The generated command installs Pi from the verified tarball via pinned npm.

    A fake npm replaces the pinned one inside the Node archive; it emulates the
    global install and rejects any deviation from the verified command shape
    (pinned Node via PATH, verified local tarball, isolated prefix, exact flag
    set). The assertions then prove the actual npm global-prefix layout and
    idempotence without network.
    """
    node_tarball, pi_tarball = _build_fixture_tarballs(tmp_path)
    install_root = tmp_path / "install-root"
    node_dir = tmp_path / "installed-node"
    pi_prefix = tmp_path / "installed-pi"
    node_plan, pi_plan = _fixture_plans(
        tmp_path,
        node_tarball=node_tarball,
        pi_tarball=pi_tarball,
        node_dir=node_dir,
        pi_prefix=pi_prefix,
    )
    script = install_verifier_tooling_script(
        node_plan=node_plan,
        pi_plan=pi_plan,
        install_root=str(install_root),
    )

    result = subprocess.run(
        ["bash", "-lc", script],
        capture_output=True,
        text=True,
        cwd=tmp_path,
    )
    assert result.returncode == 0, result.stderr
    # Node stays a verified archive extraction: bin/node at the extraction
    # root, sentinel touched (plain file, not executable).
    assert (node_dir / "bin" / "node").is_file()
    assert (node_dir / "bin" / "node").stat().st_mode & 0o111
    sentinel = node_dir / ".installed"
    assert sentinel.is_file()
    assert not (sentinel.stat().st_mode & 0o111)
    # Pi landed in the actual npm global-prefix layout: launcher under
    # $prefix/bin and the package under
    # $prefix/lib/node_modules/@earendil-works/pi-coding-agent.
    pi_bin = pi_prefix / "bin" / "pi"
    assert pi_bin.is_file()
    assert pi_bin.stat().st_mode & 0o111
    cli = (
        pi_prefix
        / "lib"
        / "node_modules"
        / "@earendil-works"
        / "pi-coding-agent"
        / "dist"
        / "cli.js"
    )
    assert cli.is_file()
    assert cli.stat().st_mode & 0o111
    # The pinned npm ran exactly once with the right shape (the fake npm
    # rejects wrong shapes with a nonzero exit), and the temporary tarball
    # staging files were removed after success.
    assert (pi_prefix / ".fake-npm-calls").read_text().splitlines() == ["call"]
    assert list(install_root.glob(".*.tgz.*")) == []

    # Idempotence without network: delete the fixture archives and rerun. The
    # layout checks must skip every download and npm invocation; markers placed
    # after install survive.
    node_tarball.unlink()
    pi_tarball.unlink()
    marker = cli.parent / "reviewer-marker"
    marker.write_text("keep")
    second = subprocess.run(
        ["bash", "-lc", script],
        capture_output=True,
        text=True,
        cwd=tmp_path,
    )
    assert second.returncode == 0, second.stderr
    assert second.stdout.count("already installed") == 2
    assert marker.read_text() == "keep"
    assert (pi_prefix / ".fake-npm-calls").read_text().splitlines() == ["call"]


def test_tooling_install_script_rejects_node_digest_mismatch(tmp_path) -> None:
    from dataclasses import replace

    node_tarball, pi_tarball = _build_fixture_tarballs(tmp_path)
    install_root = tmp_path / "install-root"
    node_dir = tmp_path / "installed-node"
    pi_prefix = tmp_path / "installed-pi"
    node_plan, pi_plan = _fixture_plans(
        tmp_path,
        node_tarball=node_tarball,
        pi_tarball=pi_tarball,
        node_dir=node_dir,
        pi_prefix=pi_prefix,
    )
    node_plan = replace(node_plan, expected_digest="0" * 64)
    script = install_verifier_tooling_script(
        node_plan=node_plan,
        pi_plan=pi_plan,
        install_root=str(install_root),
    )

    result = subprocess.run(
        ["bash", "-lc", script],
        capture_output=True,
        text=True,
        cwd=tmp_path,
    )
    assert result.returncode == 1
    assert "Node.js digest mismatch" in result.stderr
    assert not node_dir.exists()
    assert not (pi_prefix / "bin" / "pi").exists()
    assert list(install_root.glob(".*.tgz.*")) == []


def test_tooling_install_script_rejects_pi_digest_mismatch_and_cleans_staging(
    tmp_path,
) -> None:
    from dataclasses import replace

    node_tarball, pi_tarball = _build_fixture_tarballs(tmp_path)
    install_root = tmp_path / "install-root"
    node_dir = tmp_path / "installed-node"
    pi_prefix = tmp_path / "installed-pi"
    node_plan, pi_plan = _fixture_plans(
        tmp_path,
        node_tarball=node_tarball,
        pi_tarball=pi_tarball,
        node_dir=node_dir,
        pi_prefix=pi_prefix,
    )
    pi_plan = replace(pi_plan, expected_digest="0" * 88)
    script = install_verifier_tooling_script(
        node_plan=node_plan,
        pi_plan=pi_plan,
        install_root=str(install_root),
    )

    result = subprocess.run(
        ["bash", "-lc", script],
        capture_output=True,
        text=True,
        cwd=tmp_path,
    )
    assert result.returncode == 1
    assert "Pi coding agent digest mismatch" in result.stderr
    # The pinned npm was never invoked: no prefix layout, no call marker.
    assert not (pi_prefix / "bin" / "pi").exists()
    assert not (
        pi_prefix / "lib" / "node_modules" / "@earendil-works" / "pi-coding-agent"
    ).exists()
    assert not (pi_prefix / ".fake-npm-calls").exists()
    # Failure cleanup removed the temporary tarball staging; the already
    # installed Node extraction stays in place for the retry.
    assert list(install_root.glob(".*.tgz.*")) == []
    assert (node_dir / "bin" / "node").is_file()


def test_tooling_install_script_cleans_partial_prefix_when_npm_fails(
    tmp_path,
) -> None:
    import os

    node_tarball, pi_tarball = _build_fixture_tarballs(tmp_path)
    install_root = tmp_path / "install-root"
    node_dir = tmp_path / "installed-node"
    pi_prefix = tmp_path / "installed-pi"
    node_plan, pi_plan = _fixture_plans(
        tmp_path,
        node_tarball=node_tarball,
        pi_tarball=pi_tarball,
        node_dir=node_dir,
        pi_prefix=pi_prefix,
    )
    script = install_verifier_tooling_script(
        node_plan=node_plan,
        pi_plan=pi_plan,
        install_root=str(install_root),
    )

    env = {**os.environ, "FAKE_NPM_FAIL": "1"}
    result = subprocess.run(
        ["bash", "-lc", script],
        capture_output=True,
        text=True,
        cwd=tmp_path,
        env=env,
    )
    assert result.returncode != 0
    assert "fake npm failing as configured" in result.stderr
    # Both the temporary tarball and the partial prefix staging were removed.
    assert list(install_root.glob(".*.tgz.*")) == []
    assert not (
        pi_prefix / "lib" / "node_modules" / "@earendil-works" / "pi-coding-agent"
    ).exists()
    assert not (pi_prefix / "bin" / "pi").exists()
    assert (node_dir / "bin" / "node").is_file()


def test_verdict_parser_keeps_outcomes_distinct(tmp_path) -> None:
    from dorf.workflows.coding_store import CodingCommandRun

    run = CodingCommandRun(1, "j", "k", "c", "succeeded", 0, "t", "t", "", None, None)
    assert _parse_verifier_verdict(run, "") == ("infrastructure", "no_output")
    assert _parse_verifier_verdict(
        run, f"{REVIEW_NO_FINDINGS_SENTINEL}\n"
    ) == ("no-findings", None)
    assert _parse_verifier_verdict(run, "finding: off by one") == ("findings", None)
    # A successful output ending in the sentinel is still findings: the complete
    # response must be exactly the sentinel for a clean verdict.
    assert _parse_verifier_verdict(
        run, f"checked everything\n{REVIEW_NO_FINDINGS_SENTINEL}"
    ) == ("findings", None)
    assert _parse_verifier_verdict(
        run, f"{REVIEW_NO_FINDINGS_SENTINEL} trailing"
    ) == ("findings", None)
    timed = CodingCommandRun(2, "j", "k", "c", "timed_out", None, "t", "t", "", None, None)
    assert _parse_verifier_verdict(timed, "x") == ("infrastructure", "timed_out")
    failed = CodingCommandRun(3, "j", "k", "c", "failed", 1, "t", "t", "", None, None)
    assert _parse_verifier_verdict(failed, "x") == (
        "infrastructure",
        "reviewer_process_failure",
    )


def test_findings_followed_by_sentinel_are_still_findings(tmp_path) -> None:
    finding = "path: src/x.py\npossible overflow\n"
    coordinator, _, _, dorf = make_coordinator(
        tmp_path,
        review_output=f"{finding}\n{REVIEW_NO_FINDINGS_SENTINEL}",
    )
    outcome = coordinator.run()

    assert outcome.run.verdict == "findings"
    assert outcome.run.feedback_input_id is not None
    assert len(dorf.messages) == 1
    assert finding.strip() in dorf.messages[0][1]


def test_findings_packet_is_bounded_while_artifact_stays_complete(tmp_path) -> None:
    verbose_finding = ("line: detail\n" * 3000).strip()
    coordinator, store, _, dorf = make_coordinator(
        tmp_path, review_output=verbose_finding
    )
    outcome = coordinator.run()

    assert outcome.run.verdict == "findings"
    assert len(dorf.messages) == 1
    text = dorf.messages[0][1]
    assert len(text) <= len(verbose_finding) + 500
    assert "truncated" in text
    assert len(verbose_finding) > VERIFIER_FINDINGS_CHAR_LIMIT
    command_run = store.get_command_run(outcome.run.command_run_id)
    assert command_run is not None and command_run.output_path
    assert Path(command_run.output_path).read_text() == verbose_finding


def test_findings_message_bounds_the_packet_without_touching_short_findings() -> None:
    short = "path: a.py\nissue\n"
    message = verifier_findings_message(role="diff", commit=COMMIT, findings=short)
    assert short.strip() in message
    assert "truncated" not in message
    long = "x" * (VERIFIER_FINDINGS_CHAR_LIMIT + 1000)
    bounded = verifier_findings_message(role="diff", commit=COMMIT, findings=long)
    assert len(bounded) < len(long)
    assert "truncated" in bounded
    assert bounded.endswith("retained with the run evidence]")


def test_findings_message_returns_through_job_fifo_without_authorizing_merge(
    tmp_path,
) -> None:
    coordinator, store, job, dorf = make_coordinator(tmp_path, review_output="finding text")
    outcome = coordinator.run()
    assert outcome.run.verdict == "findings"
    text = dorf.messages[0][1]
    assert "cannot approve or block" in text
    assert "deterministic checks and human acceptance remain the authority" in text
    assert "finding text" in text


def test_shadow_findings_are_retained_and_never_delivered(tmp_path) -> None:
    finding = "path: src/x.py line 12\npossible integer overflow\n"
    coordinator, store, job, dorf = make_coordinator(
        tmp_path,
        review_output=finding,
        authority="shadow",
    )
    outcome = coordinator.run()

    assert outcome.exit_code == 0
    assert outcome.run.verdict == "findings"
    assert outcome.run.authority == "shadow"
    assert outcome.run.feedback_input_id is None
    assert dorf.messages == []  # nothing ever reaches the implementation Job FIFO
    assert outcome.run.cleanup_status == "clean"
    # CLI messages state the authority explicitly and say findings were retained.
    assert "verifier shadow diff run" in outcome.messages[0]
    assert any(
        "shadow findings retained as verifier evidence" in message
        and "not delivered to the Job FIFO" in message
        for message in outcome.messages
    )
    # The retained evidence is the complete command-run artifact.
    command_run = store.get_command_run(outcome.run.command_run_id)
    assert command_run is not None and command_run.output_path
    assert Path(command_run.output_path).read_text() == finding
    # The verdict event carries the shadow authority and empty delivery link.
    verdicts = [
        event
        for event in store.documents.list_events(job.job_name)
        if event.kind == "verifier-verdict"
    ]
    assert len(verdicts) == 1
    assert verdicts[0].related["authority"] == "shadow"
    assert verdicts[0].related["input"] == ""

    # Reconciliation stays idempotent: still no delivery on a rerun.
    second = coordinator.run()
    assert second.run.id == outcome.run.id
    assert dorf.messages == []
    assert second.run.feedback_input_id is None


def test_advisory_findings_are_delivered_exactly_once_across_reconciliation(
    tmp_path,
) -> None:
    coordinator, store, job, dorf = make_coordinator(
        tmp_path, review_output="finding: advisory delivery"
    )
    outcome = coordinator.run()
    assert outcome.run.authority == "advisory"
    assert outcome.run.feedback_input_id is not None
    assert len(dorf.messages) == 1
    assert "authority: advisory" in dorf.messages[0][1]

    coordinator.run()
    coordinator.run()
    assert len(dorf.messages) == 1
    assert len(dorf.spawns) == 1
    verdicts = [
        event
        for event in store.documents.list_events(job.job_name)
        if event.kind == "verifier-verdict"
    ]
    assert len(verdicts) == 1
    assert verdicts[0].related["authority"] == "advisory"
    assert verdicts[0].related["input"] == outcome.run.feedback_input_id


def test_config_change_creates_a_fresh_run_without_reusing_old_verdict(
    tmp_path,
) -> None:
    store, job = make_job_and_store(tmp_path)
    dorf = FakeDorf()
    dorf.review_output = "finding: first configuration"
    role_advisory = make_role()  # advisory, deepseek-v4-flash
    first = VerifierCoordinator(
        store=store,
        job=job,
        role=role_advisory,
        dorf=dorf,
        gateway=FakeGateway(),
        github_client=FakeGitHub(),
        token_provider=lambda: "t",
    ).run()
    assert first.run.verdict == "findings"
    assert len(dorf.messages) == 1  # delivered once under the advisory config

    # The typed configuration changes: authority, model, reasoning, prompt.
    role_shadow = VerifierRole(
        name="diff",
        harness="pi",
        connection="deepseek",
        model="deepseek-v4-pro",
        reasoning_effort="high",
        authority="shadow",
        room="dedicated",
        timeout_seconds=120,
        prompt="Review with a different strict rubric.",
    )
    dorf.review_output = "finding: second configuration"
    second = VerifierCoordinator(
        store=store,
        job=job,
        role=role_shadow,
        dorf=dorf,
        gateway=FakeGateway(),
        github_client=FakeGitHub(),
        token_provider=lambda: "t",
    ).run()

    # Fresh run under the new digest: never the old run, never its verdict.
    assert second.run.id != first.run.id
    assert second.run.config_digest == verifier_config_digest(role_shadow)
    assert second.run.config_digest != first.run.config_digest
    assert second.run.worker_name != first.run.worker_name
    assert second.run.verdict == "findings"
    assert second.run.authority == "shadow"
    assert second.run.feedback_input_id is None
    # The old delivery is untouched and not duplicated by the new run.
    assert len(dorf.messages) == 1
    assert dorf.messages[0][3] == first.run.feedback_input_id
    old = store.get_verifier_run(first.run.id)
    assert old.verdict == "findings"
    assert old.config_digest == first.run.config_digest
    # Both runs coexist under their own digests; the new one cleaned its Room.
    assert second.run.cleanup_status == "clean"


def test_config_change_supersedes_a_running_run_of_the_old_config(tmp_path) -> None:
    store, job = make_job_and_store(tmp_path)
    dorf = FakeDorf()
    role_a = make_role()
    worker_a = verifier_worker_name(
        job.job_name,
        "diff",
        COMMIT,
        1,
        config_digest=verifier_config_digest(role_a),
    )
    stale = make_run(
        store,
        job_name=job.job_name,
        role=role_a,
        worker_name=worker_a,
        room_id="room-a",
        route_id="route-a",
    )
    dorf.bindings[worker_a] = FakeBinding(worker_a)
    dorf.review_output = REVIEW_NO_FINDINGS_SENTINEL
    role_b = make_role(authority="shadow")
    coordinator = VerifierCoordinator(
        store=store,
        job=job,
        role=role_b,
        dorf=dorf,
        gateway=FakeGateway(),
        github_client=FakeGitHub(),
        token_provider=lambda: "t",
    )
    outcome = coordinator.run()

    superseded = store.get_verifier_run(stale.id)
    assert superseded.status == "infrastructure"
    assert superseded.failure_kind == "config_changed"
    assert superseded.cleanup_status == "clean"
    assert superseded.cleanup_route_revoked is True
    assert superseded.cleanup_room_gone is True
    assert outcome.run.id != stale.id
    assert outcome.run.verdict == "no-findings"
    assert outcome.run.authority == "shadow"
    assert dorf.ended[0] == (worker_a, False)  # the old worker was retired
    # The supersession itself is a persisted verdict event with its own digest.
    verdicts = [
        event
        for event in store.documents.list_events(job.job_name)
        if event.kind == "verifier-verdict"
    ]
    assert {event.related["verdict"] for event in verdicts} == {
        "no-findings",
        "infrastructure",
    }
    superseded_event = next(
        event
        for event in verdicts
        if event.related["run"] == str(stale.id)
    )
    assert superseded_event.related["config_digest"] == verifier_config_digest(role_a)
    assert superseded_event.related["authority"] == "advisory"


def test_worker_identity_includes_the_config_digest(tmp_path) -> None:
    advisory = make_role()
    shadow = make_role(authority="shadow")
    other_model = make_role(model="deepseek-v4-pro")
    base = verifier_worker_name("checkout-perf", "diff", COMMIT, 1)
    a = verifier_worker_name(
        "checkout-perf",
        "diff",
        COMMIT,
        1,
        config_digest=verifier_config_digest(advisory),
    )
    assert a != base
    assert len(a) <= 63  # runtime Worker name bound
    assert a == verifier_worker_name(
        "checkout-perf",
        "diff",
        COMMIT,
        1,
        config_digest=verifier_config_digest(advisory),
    )
    # Shadow vs advisory, model, or any typed field change -> distinct identity.
    assert a != verifier_worker_name(
        "checkout-perf",
        "diff",
        COMMIT,
        1,
        config_digest=verifier_config_digest(shadow),
    )
    assert a != verifier_worker_name(
        "checkout-perf",
        "diff",
        COMMIT,
        1,
        config_digest=verifier_config_digest(other_model),
    )


def test_config_snapshot_and_digest_cover_the_complete_typed_configuration() -> None:
    role = make_role()
    snapshot = verifier_config_snapshot(role)
    import json

    parsed = json.loads(snapshot)
    assert parsed["role"] == "diff"
    assert parsed["harness"] == "pi"
    assert parsed["connection"] == "deepseek-review"
    assert parsed["model"] == MODEL
    assert parsed["reasoning_effort"] == "max"
    assert parsed["authority"] == "advisory"
    assert parsed["room"] == "dedicated"
    assert parsed["timeout_seconds"] == 120
    assert parsed["prompt"] == role.prompt
    digest = verifier_config_digest(role)
    assert digest == "sha256:" + hashlib.sha256(snapshot.encode()).hexdigest()
    # Canonical: identical roles produce identical snapshots and digests.
    assert verifier_config_snapshot(make_role()) == snapshot
    assert verifier_config_digest(make_role()) == digest
    # Each typed field change moves the digest.
    for changed in (
        make_role(authority="shadow"),
        make_role(model="deepseek-v4-pro"),
        make_role(reasoning_effort="high"),
        make_role(connection="deepseek"),
        make_role(timeout_seconds=60),
        make_role(prompt="A different prompt."),
    ):
        assert verifier_config_digest(changed) != digest


def test_verdict_event_carries_typed_config_provenance(tmp_path) -> None:
    coordinator, store, job, _ = make_coordinator(
        tmp_path, review_output=REVIEW_NO_FINDINGS_SENTINEL
    )
    outcome = coordinator.run()

    verdicts = [
        event
        for event in store.documents.list_events(job.job_name)
        if event.kind == "verifier-verdict"
    ]
    assert len(verdicts) == 1
    related = verdicts[0].related
    assert related["authority"] == "advisory"
    assert related["config_digest"] == verifier_config_digest(make_role())
    assert related["config_harness"] == "pi"
    assert related["config_connection"] == "deepseek-review"
    assert related["config_model"] == MODEL
    assert related["config_reasoning_effort"] == "max"
    assert related["config_room"] == "dedicated"
    assert related["config_timeout_seconds"] == "120"
    assert related["config_prompt_digest"]
    assert "advisory diff verifier observed no-findings" in verdicts[0].summary
    # The persisted run row keeps the full auditable snapshot.
    run = store.get_verifier_run(outcome.run.id)
    assert run.authority == "advisory"
    assert run.config_digest == related["config_digest"]
    assert run.config_snapshot == verifier_config_snapshot(make_role())


def test_verifier_execution_never_calls_the_codex_authentication_probe(
    tmp_path,
) -> None:
    store, job = make_job_and_store(tmp_path)
    dorf = FakeDorf()

    class NoProbeExecution(FakeExecution):
        def check_codex_authentication(self) -> None:
            raise AssertionError(
                "verifier must never call the Codex authentication probe"
            )

    executions: dict[str, NoProbeExecution] = {}

    def worker_execution(name):
        execution = executions.get(name)
        if execution is None:
            execution = NoProbeExecution(dorf.bindings[name], dorf)
            executions[name] = execution
        return execution

    dorf.worker_execution = worker_execution
    dorf.review_output = REVIEW_NO_FINDINGS_SENTINEL
    coordinator = VerifierCoordinator(
        store=store,
        job=job,
        role=make_role(),
        dorf=dorf,
        gateway=FakeGateway(),
        github_client=FakeGitHub(),
        token_provider=lambda: "t",
    )
    outcome = coordinator.run()

    assert outcome.run.verdict == "no-findings"
    assert outcome.run.cleanup_status == "clean"
    # Route/provider failure is classified from the exact prefixed Pi
    # invocation: only the one prefixed DeepSeek model command is processed.
    processed = executions[dorf.spawns[0][0]].processed
    assert len(processed) == 1
    pi_command_argv = processed[0][0]
    assert "--model dorf-deepseek/deepseek/deepseek-v4-flash" in pi_command_argv[2]
    assert "chatgpt" not in pi_command_argv[2].casefold()
    assert "--provider dorf-deepseek" in pi_command_argv[2]


def test_prefixed_pi_failure_is_classified_without_any_probe(tmp_path) -> None:
    """A failing exact prefixed Pi invocation is infrastructure, not findings."""
    coordinator, _, _, dorf = make_coordinator(tmp_path, review_output="partial")
    dorf.review_output = "unfinished"
    dorf.review_exit_code = 1
    outcome = coordinator.run()

    assert outcome.run.verdict == "infrastructure"
    assert outcome.run.failure_kind == "reviewer_process_failure"
    assert dorf.messages == []
    assert dorf.authentication_checks == []


def test_experimental_verifier_table_without_config_columns_fails_clearly(
    tmp_path,
) -> None:
    import sqlite3

    path = tmp_path / "old-state.sqlite3"
    connection = sqlite3.connect(path)
    connection.execute(
        """
        CREATE TABLE verifier_runs (
            id INTEGER PRIMARY KEY,
            job_name TEXT NOT NULL,
            role TEXT NOT NULL,
            commit_sha TEXT NOT NULL,
            generation INTEGER NOT NULL DEFAULT 1,
            worker_name TEXT NOT NULL,
            room_id TEXT NOT NULL,
            route_id TEXT NOT NULL,
            status TEXT NOT NULL,
            verdict TEXT,
            failure_kind TEXT,
            command_run_id INTEGER,
            feedback_input_id TEXT,
            commit_before TEXT,
            commit_after TEXT,
            tree_before TEXT,
            tree_after TEXT,
            worktree_before TEXT,
            worktree_after TEXT,
            cleanup_status TEXT NOT NULL,
            cleanup_route_revoked INTEGER NOT NULL DEFAULT 0,
            cleanup_room_gone INTEGER NOT NULL DEFAULT 0,
            created_at TEXT NOT NULL,
            updated_at TEXT NOT NULL,
            UNIQUE(job_name, role, commit_sha, generation)
        )
        """
    )
    connection.commit()
    connection.close()

    with pytest.raises(RuntimeError, match="verifier_runs table is missing columns"):
        CodingStore.open(path)

    # A freshly created table gets the new shape and works.
    fresh = CodingStore.open(tmp_path / "fresh.sqlite3")
    run = fresh.create_verifier_run(
        job_name="checkout-perf",
        role="diff",
        commit_sha=COMMIT,
        generation=1,
        worker_name="verifier-fresh",
        room_id="",
        route_id="",
        authority="shadow",
        config_digest="sha256:" + "0" * 64,
        config_snapshot='{"role":"diff"}',
    )
    assert run.authority == "shadow"
    assert run.config_digest == "sha256:" + "0" * 64
    # Same identity reserves the same row; a different digest reserves a fresh
    # row even at the same generation.
    again = fresh.create_verifier_run(
        job_name="checkout-perf",
        role="diff",
        commit_sha=COMMIT,
        generation=1,
        worker_name="verifier-fresh",
        room_id="",
        route_id="",
        authority="shadow",
        config_digest="sha256:" + "0" * 64,
        config_snapshot='{"role":"diff"}',
    )
    assert again.id == run.id
    other = fresh.create_verifier_run(
        job_name="checkout-perf",
        role="diff",
        commit_sha=COMMIT,
        generation=1,
        worker_name="verifier-fresh-other",
        room_id="",
        route_id="",
        authority="advisory",
        config_digest="sha256:" + "1" * 64,
        config_snapshot='{"role":"diff","authority":"advisory"}',
    )
    assert other.id != run.id
