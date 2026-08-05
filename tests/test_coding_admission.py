from __future__ import annotations

import re
import subprocess
from dataclasses import replace
from pathlib import Path
from types import SimpleNamespace

from dorf.adapters.agents.codex_config import CodexConfig
from dorf.adapters.environments import IncusCheckResult, IncusConfig, IncusFailure
from dorf.coding_workspace import GitAuthorIdentity
from dorf.deployment_profile import DeploymentProfile
from dorf.github_app import (
    GitHubInstallationToken,
    GitHubIssue,
    GitHubRepositoryClient,
    GitHubRepositoryError,
)
from dorf.provider_gateway import InferenceRoute
from dorf.repo_contract import RepoContract, ReviewAgent, ReviewConfig
from dorf.workflows import (
    AdmissionFailure,
    CodingAdmissionPreflight,
    CodingAdmissionProof,
    CodingAdmissionRequest,
    GitHubAuthorityApproval,
)
from dorf.workflows.coding_admission import (
    ADMISSION_WORKSPACE,
    LocalCodingAdmissionBackend,
    _missing_app_permissions,
)


def proof() -> CodingAdmissionProof:
    return CodingAdmissionProof.create(
        repository="example/repo",
        installation_id="123",
        issue=GitHubIssue(18, "One admission proof", "Body", ()),
        target_branch="main",
        target_start_sha="a" * 40,
        image_fingerprint="b" * 64,
        provider_connection="personal-chatgpt",
        reviewer="codex",
        contract=RepoContract(
            mode="configured",
            commands={"prepare": "true", "smoke": "true"},
            env={},
            review=ReviewConfig(
                agents={
                    "codex": ReviewAgent(
                        "codex",
                        "codex exec {dorf_review_prompt}",
                    )
                }
            ),
        ),
        codex_config=CodexConfig("gpt-5.6-sol", "low"),
        git_author=GitAuthorIdentity("Dorf", "dorf@example.com"),
        environment_config=IncusConfig(template="b" * 64),
        installation_token="secret-token",
    )


class Backend:
    def __init__(self, failures: dict[str, tuple[AdmissionFailure, ...]] | None = None) -> None:
        self.failures = failures or {}
        self.calls: list[str] = []

    def check_repository(self, request):
        self.calls.append("repository")
        return self.failures.get("repository", ())

    def check_github(self, request):
        self.calls.append("github")
        return self.failures.get("github", ())

    def check_platform(self, request):
        self.calls.append("platform")
        return self.failures.get("platform", ())

    def exercise_consumer_path(self, request):
        self.calls.append("consumer")
        return self.failures.get("consumer", ())

    def proof(self, request):
        self.calls.append("proof")
        return proof()


def failure(code: str, owner: str) -> AdmissionFailure:
    return AdmissionFailure(
        code=code,
        owner=owner,
        summary=f"{code} failed",
        repair=f"repair {code}",
        consequence=f"cannot use {code}",
        automatic_continuation=False,
    )


def test_preflight_returns_all_independently_discoverable_failures() -> None:
    backend = Backend(
        {
            "repository": (failure("repository-dirty", "repository-owner"),),
            "github": (failure("github-authority", "github-owner"),),
            "platform": (
                failure("official-image", "incus-owner"),
                failure("provider-route", "provider-owner"),
            ),
        }
    )

    result = CodingAdmissionPreflight(backend).prove(
        CodingAdmissionRequest(repo_path="/repo", target_branch="main", issue_number=18)
    )

    assert result.proof is None
    assert [item.code for item in result.failures] == [
        "repository-dirty",
        "github-authority",
        "official-image",
        "provider-route",
    ]
    assert all(item.owner and item.repair and item.consequence for item in result.failures)
    assert backend.calls == ["repository", "github", "platform"]


def test_preflight_exercises_real_consumer_path_and_returns_one_reusable_proof() -> None:
    backend = Backend()
    request = CodingAdmissionRequest(
        repo_path="/repo",
        target_branch="main",
        issue_number=18,
        provider_connection="personal-chatgpt",
    )

    result = CodingAdmissionPreflight(backend).prove(request)

    assert result.failures == ()
    assert result.proof == proof()
    assert result.proof.record()["proof_id"] == proof().proof_id
    assert "installation_token" not in result.proof.record()
    assert backend.calls == ["repository", "github", "platform", "consumer", "proof"]
    assert replace(result.proof, installation_token="rotated").proof_id == result.proof.proof_id


def test_repository_check_rejects_a_changed_pinned_delegation_start(monkeypatch) -> None:
    def git(repo, *args):
        values = {
            ("status", "--porcelain"): "",
            ("rev-parse", "HEAD"): "b" * 40 + "\n",
            ("remote", "get-url", "origin"): "https://github.com/example/repo.git\n",
            ("config", "user.name"): "Dorf\n",
            ("config", "user.email"): "dorf@example.com\n",
        }
        return subprocess.CompletedProcess(args, 0, values[args], "")

    monkeypatch.setattr("dorf.workflows.coding_admission._git", git)
    monkeypatch.setattr(
        "dorf.workflows.coding_admission.load_repo_contract", lambda repo: proof().contract
    )

    failures = LocalCodingAdmissionBackend().check_repository(
        CodingAdmissionRequest(
            repo_path="/repo",
            target_branch="main",
            issue_number=20,
            command="afk",
            target_start_sha="a" * 40,
        )
    )

    assert [failure.code for failure in failures] == ["delegation-start-changed"]


def test_repository_check_rejects_changed_pinned_github_repository(monkeypatch) -> None:
    def git(repo, *args):
        values = {
            ("status", "--porcelain"): "",
            ("remote", "get-url", "origin"): "https://github.com/other/repo.git\n",
            ("config", "user.name"): "Dorf\n",
            ("config", "user.email"): "dorf@example.com\n",
        }
        return subprocess.CompletedProcess(args, 0, values[args], "")

    monkeypatch.setattr("dorf.workflows.coding_admission._git", git)
    monkeypatch.setattr(
        "dorf.workflows.coding_admission.load_repo_contract", lambda repo: proof().contract
    )

    failures = LocalCodingAdmissionBackend().check_repository(
        CodingAdmissionRequest(
            repo_path="/repo",
            target_branch="main",
            issue_number=20,
            repository="example/repo",
        )
    )

    assert [failure.code for failure in failures] == ["delegation-repository-changed"]


def test_github_permission_write_satisfies_required_read_authority() -> None:
    assert _missing_app_permissions(
        {
            "contents": "write",
            "issues": "write",
            "metadata": "read",
            "pull_requests": "admin",
        }
    ) == []


def test_github_missing_exact_repository_authority_is_one_resumable_approval(
    monkeypatch,
    tmp_path,
) -> None:
    backend = LocalCodingAdmissionBackend()
    backend.repo = tmp_path
    backend.repository = "example/repo"
    monkeypatch.setattr(
        "dorf.workflows.coding_admission.load_github_app_config",
        lambda: SimpleNamespace(installation_id="123"),
    )
    monkeypatch.setattr(
        "dorf.workflows.coding_admission.GitHubAppTokenClient.mint_installation_token",
        lambda self, config: GitHubInstallationToken(
            token="installation-token",
            permissions={
                "contents": "write",
                "issues": "read",
                "metadata": "read",
                "pull_requests": "write",
            },
        ),
    )
    monkeypatch.setattr(
        "dorf.workflows.coding_admission.GitHubRepositoryClient.get_branch_sha",
        lambda self, repository, branch: (_ for _ in ()).throw(
            GitHubRepositoryError("HTTP 404: Not Found", status_code=404)
        ),
    )

    failures = backend.check_github(
        CodingAdmissionRequest(
            repo_path=str(tmp_path),
            target_branch="main",
            issue_number=20,
        )
    )

    assert len(failures) == 1
    failure = failures[0]
    assert failure.code == "github-repository-authority"
    assert failure.automatic_continuation is True
    assert failure.approval == GitHubAuthorityApproval(
        installation_id="123",
        missing_authority=(
            "Persistent repository access for Dorf GitHub App installation 123 to example/repo"
        ),
        why_needed=(
            "Coding admission must read issue #20 and main, write the Job branch, and manage its "
            "pull request."
        ),
        action=(
            "In GitHub settings, select example/repo for Dorf GitHub App installation 123 and "
            "save. This grants that installation persistent repository-wide access with its "
            "configured permissions until an owner changes or removes the access."
        ),
        scope=(
            "Persistent repository-wide authority on example/repo for installation 123: metadata "
            "read, issues read, contents write, and pull requests write. It is not limited to this "
            "issue, branch, or delegation."
        ),
        approve_consequence=(
            "Dorf GitHub App installation 123 gains those configured permissions across "
            "example/repo; Dorf can use them while that repository access remains installed."
        ),
        decline_consequence=(
            "Dorf GitHub App installation 123 does not gain access to example/repo; this pending "
            "delegation ends without creating a Job, branch, Room, or pull request."
        ),
        automatic_resume=(
            "Dorf polls only installation 123. When that installation can read example/repo:main, "
            "Dorf reruns exact readiness and continues this delegation automatically; a different "
            "configured installation cannot approve it."
        ),
        url="https://github.com/settings/installations/123",
        repository="example/repo",
    )
    authority_text = " ".join(
        (
            failure.approval.action,
            failure.approval.scope,
            failure.approval.approve_consequence,
        )
    )
    assert "persistent repository-wide" in authority_text
    assert all(
        permission in authority_text
        for permission in ("issues read", "contents write", "pull requests write")
    )
    assert "not limited to this issue, branch, or delegation" in authority_text


def test_github_readiness_rejects_a_pinned_installation_configuration_swap(
    monkeypatch, tmp_path
) -> None:
    backend = LocalCodingAdmissionBackend()
    backend.repo = tmp_path
    backend.repository = "example/repo"
    monkeypatch.setattr(
        "dorf.workflows.coding_admission.load_github_app_config",
        lambda: SimpleNamespace(installation_id="456", app_slug="dorf-local"),
    )
    minted = []
    monkeypatch.setattr(
        "dorf.workflows.coding_admission.GitHubAppTokenClient.mint_installation_token",
        lambda self, config: minted.append(config),
    )

    failures = backend.check_github(
        CodingAdmissionRequest(
            repo_path=str(tmp_path),
            target_branch="main",
            issue_number=20,
            repository="example/repo",
            installation_id="123",
        )
    )

    assert [failure.code for failure in failures] == [
        "delegation-installation-changed"
    ]
    assert minted == []
    assert backend.installation_token is None


def test_github_check_accepts_installation_authority_when_repository_user_push_is_false(
    monkeypatch,
    tmp_path,
) -> None:
    subprocess.run(["git", "init", "-b", "main", str(tmp_path)], check=True, capture_output=True)
    subprocess.run(
        ["git", "-C", str(tmp_path), "config", "user.name", "Dorf"],
        check=True,
    )
    subprocess.run(
        ["git", "-C", str(tmp_path), "config", "user.email", "dorf@example.com"],
        check=True,
    )
    (tmp_path / "README.md").write_text("ready\n")
    subprocess.run(["git", "-C", str(tmp_path), "add", "README.md"], check=True)
    subprocess.run(
        ["git", "-C", str(tmp_path), "commit", "-m", "Initial"],
        check=True,
        capture_output=True,
    )
    head = subprocess.run(
        ["git", "-C", str(tmp_path), "rev-parse", "HEAD"],
        check=True,
        capture_output=True,
        text=True,
    ).stdout.strip()
    client = GitHubRepositoryClient("installation-token")
    requested_paths = []

    def request_json(method, path):
        requested_paths.append(path)
        responses = {
            "/repos/example/repo": {
                "permissions": {"pull": True, "push": False},
            },
            "/repos/example/repo/git/ref/heads/main": {"object": {"sha": head}},
            "/repos/example/repo/issues/18": {
                "number": 18,
                "title": "Replace scattered health checks",
                "body": "Use one admission proof.",
            },
            "/repos/example/repo/issues/18/comments?per_page=100&page=1": [],
        }
        return responses[path]

    monkeypatch.setattr(client, "_request_json", request_json)
    monkeypatch.setattr(
        "dorf.workflows.coding_admission.load_github_app_config",
        lambda: SimpleNamespace(installation_id="123", app_slug="dorf-local"),
    )
    monkeypatch.setattr(
        "dorf.workflows.coding_admission.GitHubAppTokenClient.mint_installation_token",
        lambda self, config: GitHubInstallationToken(
            token="installation-token",
            permissions={
                "contents": "write",
                "issues": "read",
                "metadata": "read",
                "pull_requests": "write",
            },
        ),
    )
    monkeypatch.setattr(
        "dorf.workflows.coding_admission.GitHubRepositoryClient",
        lambda token: client,
    )
    backend = LocalCodingAdmissionBackend()
    backend.repo = tmp_path
    backend.repository = "example/repo"

    failures = backend.check_github(
        CodingAdmissionRequest(
            repo_path=str(tmp_path),
            target_branch="main",
            issue_number=18,
        )
    )

    assert failures == ()
    assert backend.target_start_sha == head
    assert backend.issue == GitHubIssue(
        18,
        "Replace scattered health checks",
        "Use one admission proof.",
        (),
    )
    assert requested_paths == [
        "/repos/example/repo/git/ref/heads/main",
        "/repos/example/repo/issues/18",
        "/repos/example/repo/issues/18/comments?per_page=100&page=1",
    ]


def test_platform_check_repairs_missing_pinned_image_with_setup_and_repin(
    monkeypatch,
) -> None:
    fingerprint = "b" * 64
    backend = LocalCodingAdmissionBackend(gateway=DisposableGateway())
    backend.contract = proof().contract
    monkeypatch.setattr(
        "dorf.workflows.coding_admission.load_deployment_profile",
        lambda: DeploymentProfile(
            provider_connection="personal-chatgpt",
            image_fingerprint=fingerprint,
        ),
    )
    monkeypatch.setattr(
        "dorf.workflows.coding_admission.Dorf.check_environment",
        lambda config, *, probe=None: IncusCheckResult(
            failures=[
                IncusFailure(
                    "incus-template",
                    f"Incus image/template not found or inaccessible: {fingerprint}",
                )
            ],
            remediation="Run `dorf doctor` and repair the Incus prerequisite.",
        ),
    )

    failures = backend.check_platform(
        CodingAdmissionRequest(repo_path="/repo", target_branch="main", issue_number=18)
    )

    assert [failure.code for failure in failures] == ["official-image-missing"]
    assert failures[0].owner == "Dorf setup owner"
    assert "dorf setup" in failures[0].repair
    assert "repin" in failures[0].repair
    assert "dorf doctor" not in failures[0].repair


def test_platform_check_reports_invalid_incus_bridge_as_provider_route_failure(
    monkeypatch,
) -> None:
    class Ipv6OnlyBridgeProbe(DisposableProbe):
        def run(self, argv, *, input=None, timeout_seconds=None):
            if argv[:3] == ["incus", "network", "get"]:
                return subprocess.CompletedProcess(argv, 0, "fd42::1/64\n", "")
            return super().run(argv, input=input, timeout_seconds=timeout_seconds)

    backend = LocalCodingAdmissionBackend(probe=Ipv6OnlyBridgeProbe())
    backend.contract = proof().contract
    profile = DeploymentProfile(
        provider_connection="personal-chatgpt",
        image_fingerprint="b" * 64,
    )
    monkeypatch.setattr(
        "dorf.workflows.coding_admission.load_deployment_profile",
        lambda: profile,
    )
    monkeypatch.setattr(
        "dorf.workflows.coding_admission.Dorf.check_environment",
        lambda config, *, probe=None: type("Result", (), {"failures": []})(),
    )
    failures = backend.check_platform(
        CodingAdmissionRequest(repo_path="/repo", target_branch="main", issue_number=18)
    )

    assert [item.code for item in failures] == ["provider-route"]
    assert "does not have a private bridge IPv4 address" in failures[0].summary


class DisposableProbe:
    def __init__(self, *, instance_exists: bool = False) -> None:
        self.commands: list[list[str]] = []
        self.invocations: list[tuple[list[str], str | None, float | None]] = []
        self.instance_exists = instance_exists

    def which(self, command: str) -> str | None:
        return f"/usr/bin/{command}"

    def run(self, argv, *, input=None, timeout_seconds=None):
        self.commands.append(argv)
        self.invocations.append((argv, input, timeout_seconds))
        if argv[:2] == ["incus", "info"]:
            return subprocess.CompletedProcess(
                argv,
                0 if self.instance_exists else 1,
                "status: RUNNING\n" if self.instance_exists else "",
                "" if self.instance_exists else "Error: Instance not found",
            )
        if argv[:2] == ["incus", "init"]:
            if self.instance_exists:
                return subprocess.CompletedProcess(argv, 1, "", "Instance already exists")
            self.instance_exists = True
        if argv[:2] == ["incus", "delete"]:
            self.instance_exists = False
        script = " ".join(argv)
        nonce = re.search(r"DORF_(?:IMPLEMENTATION|ADMISSION)_READY_[0-9a-f]{16}", script)
        stdout = f"{nonce.group(0)}\n" if nonce else ""
        return subprocess.CompletedProcess(argv, 0, stdout, "")


class DisposableGateway:
    def __init__(self) -> None:
        self.routes: dict[str, InferenceRoute] = {}

    def require_connection(self, name):
        return None

    def create_route(self, connection_name, *, consumer, wire_api="responses"):
        route = InferenceRoute(
            "route-admission",
            connection_name,
            "http://10.42.0.1:8317/v1",
            "responses",
            "temporary-route-key",
        )
        self.routes[consumer] = route
        return route

    def route_for_consumer(self, consumer):
        return self.routes.get(consumer)

    def revoke_route(self, route_id):
        consumer = next(
            (name for name, route in self.routes.items() if route.id == route_id),
            None,
        )
        if consumer is None:
            return False
        del self.routes[consumer]
        return True


class ReadyLocalBackend(LocalCodingAdmissionBackend):
    def check_repository(self, request):
        reusable = proof()
        self.repo = Path(request.repo_path)
        self.repository = reusable.repository
        self.contract = reusable.contract
        self.codex_config = reusable.codex_config
        self.git_author = reusable.git_author
        return ()

    def check_github(self, request):
        reusable = proof()
        self.installation_id = reusable.installation_id
        self.installation_token = reusable.installation_token
        self.issue = reusable.issue
        self.target_start_sha = reusable.target_start_sha
        return ()

    def check_platform(self, request):
        reusable = proof()
        self.environment_config = reusable.environment_config
        self.provider_connection = reusable.provider_connection
        self.image_fingerprint = reusable.image_fingerprint
        return ()


def install_app_server_driver(monkeypatch):
    calls = []

    class AppServerDriver:
        def __init__(self, environment):
            calls.append(("driver", environment))

        def prepare(self, binding):
            calls.append(("prepare", binding))

        def start_job_conversation(
            self,
            binding,
            turn,
            *,
            conversation_started,
            turn_prepared,
            turn_started,
            timeout_seconds=None,
        ):
            calls.append(("turn", binding, turn, timeout_seconds))
            conversation_started("thread-admission")
            turn_prepared(None)
            turn_started("turn-admission")
            return SimpleNamespace(status="completed")

    monkeypatch.setattr("dorf.sdk.CodexDriver", AppServerDriver)
    return calls


def test_local_consumer_proof_uses_app_server_and_one_disposable_vm(
    monkeypatch,
) -> None:
    driver_calls = install_app_server_driver(monkeypatch)
    probe = DisposableProbe()
    gateway = DisposableGateway()
    backend = ReadyLocalBackend(probe=probe, gateway=gateway)

    result = CodingAdmissionPreflight(backend).prove(
        CodingAdmissionRequest(repo_path="/repo", target_branch="main", issue_number=18)
    )

    assert result.ready
    assert gateway.routes == {}
    assert sum(command[:2] == ["incus", "init"] for command in probe.commands) == 1
    assert sum(command[:2] == ["incus", "delete"] for command in probe.commands) == 1
    joined = [" ".join(command) for command in probe.commands]
    assert any("git clone" in command for command in joined)
    assert any("git push --dry-run" in command for command in joined)
    assert sum("codex exec" in command for command in joined) == 1
    assert any("DORF_ADMISSION_READY_" in command for command in joined)
    reviewer_invocation = next(
        invocation
        for invocation in probe.invocations
        if "codex exec" in " ".join(invocation[0])
    )
    assert reviewer_invocation[1:] == ("", 180)
    repository_commands = [
        invocation
        for invocation in probe.invocations
        if "--cwd" in invocation[0]
        and ADMISSION_WORKSPACE in invocation[0]
        and (
            invocation[0][-2:] == ["-lc", "true"]
            or "git push --dry-run" in " ".join(invocation[0])
        )
    ]
    assert len(repository_commands) == 3
    assert all(input == "" for _, input, _ in repository_commands)
    assert [call[0] for call in driver_calls] == ["driver", "prepare", "turn"]
    _, binding, turn, timeout_seconds = driver_calls[-1]
    assert binding.environment_id.startswith("dorf-coding-admission-")
    assert binding.workspace == "/workspace/admission"
    assert turn.model == "gpt-5.6-sol"
    assert turn.reasoning_effort == "low"
    assert timeout_seconds == 180


def test_local_consumer_proof_deletes_vm_when_route_revocation_cannot_write(
    monkeypatch,
) -> None:
    install_app_server_driver(monkeypatch)
    class UnwritableRouteGateway(DisposableGateway):
        def revoke_route(self, route_id):
            raise PermissionError("provider route state is unwritable")

    probe = DisposableProbe()
    backend = ReadyLocalBackend(probe=probe, gateway=UnwritableRouteGateway())

    result = CodingAdmissionPreflight(backend).prove(
        CodingAdmissionRequest(repo_path="/repo", target_branch="main", issue_number=18)
    )

    assert [failure.code for failure in result.failures] == ["provider-route-cleanup"]
    assert sum(command[:2] == ["incus", "delete"] for command in probe.commands) == 1


def test_local_consumer_proof_preserves_preexisting_instance_on_name_collision(
    monkeypatch,
) -> None:
    monkeypatch.setattr(
        "dorf.workflows.coding_admission.secrets.token_hex",
        lambda length: "collision",
    )
    probe = DisposableProbe(instance_exists=True)
    backend = ReadyLocalBackend(probe=probe, gateway=DisposableGateway())

    result = CodingAdmissionPreflight(backend).prove(
        CodingAdmissionRequest(repo_path="/repo", target_branch="main", issue_number=18)
    )

    assert [failure.code for failure in result.failures] == ["consumer-path"]
    assert "already exists" in result.failures[0].summary
    assert sum(command[:2] == ["incus", "info"] for command in probe.commands) == 1
    assert not any(command[:2] == ["incus", "init"] for command in probe.commands)
    assert not any(command[:2] == ["incus", "delete"] for command in probe.commands)
    assert probe.instance_exists
