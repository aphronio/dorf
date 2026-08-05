from __future__ import annotations

import re
import subprocess
from dataclasses import replace
from pathlib import Path
from types import SimpleNamespace

from dorf.adapters.agents.codex_config import CodexConfig
from dorf.adapters.environments import IncusConfig
from dorf.coding_workspace import GitAuthorIdentity
from dorf.deployment_profile import DeploymentProfile
from dorf.github_app import GitHubIssue
from dorf.provider_gateway import InferenceRoute
from dorf.repo_contract import RepoContract, ReviewAgent, ReviewConfig
from dorf.workflows import (
    AdmissionFailure,
    CodingAdmissionPreflight,
    CodingAdmissionProof,
    CodingAdmissionRequest,
)
from dorf.workflows.coding_admission import (
    LocalCodingAdmissionBackend,
    _missing_app_permissions,
)


def proof() -> CodingAdmissionProof:
    return CodingAdmissionProof.create(
        repository="example/repo",
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


def test_github_permission_write_satisfies_required_read_authority() -> None:
    assert _missing_app_permissions(
        {
            "contents": "write",
            "issues": "write",
            "metadata": "read",
            "pull_requests": "admin",
        }
    ) == []


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
        "dorf.workflows.coding_admission.IncusDoctor.fast_check",
        lambda self, config: type("Result", (), {"failures": []})(),
    )
    failures = backend.check_platform(
        CodingAdmissionRequest(repo_path="/repo", target_branch="main", issue_number=18)
    )

    assert [item.code for item in failures] == ["provider-route"]
    assert "does not have a private bridge IPv4 address" in failures[0].summary


class DisposableProbe:
    def __init__(self, *, instance_exists: bool = False) -> None:
        self.commands: list[list[str]] = []
        self.instance_exists = instance_exists

    def which(self, command: str) -> str | None:
        return f"/usr/bin/{command}"

    def run(self, argv, *, input=None, timeout_seconds=None):
        self.commands.append(argv)
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

    monkeypatch.setattr("dorf.workflows.coding_admission.CodexDriver", AppServerDriver)
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
