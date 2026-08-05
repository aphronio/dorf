"""One no-durable-mutation proof for coding-workflow admission."""

from __future__ import annotations

import hashlib
import json
import os
import re
import secrets
import shlex
import subprocess
import tempfile
import time
import urllib.parse
from dataclasses import dataclass, field
from pathlib import Path
from typing import Protocol

from dorf.adapters.agents.codex import CodexDriver
from dorf.adapters.agents.codex_config import (
    AgentConfigValidationError,
    CodexConfig,
    resolve_codex_config,
)
from dorf.adapters.environments import (
    IncusConfig,
    IncusDoctor,
    IncusEnvironment,
    IncusRunnerProbe,
)
from dorf.codex_room import (
    CODEX_CONFIG_PATH,
    CODEX_ROUTE_CREDENTIAL_PATH,
    CODEX_ROUTE_ENV_KEY,
    CodexRoomEnvironment,
)
from dorf.coding_workspace import GitAuthorIdentity, git_clone_workspace_script
from dorf.deployment_profile import (
    DeploymentProfileError,
    load_deployment_profile,
)
from dorf.github_app import (
    GitHubAppConfigError,
    GitHubAppTokenClient,
    GitHubAppVerificationError,
    GitHubInstallationToken,
    GitHubIssue,
    GitHubRepositoryClient,
    GitHubRepositoryError,
    load_github_app_config,
)
from dorf.provider_gateway import ProviderGateway, ProviderGatewayError
from dorf.repo_contract import (
    REVIEW_PROMPT_PLACEHOLDER,
    ContractValidationError,
    RepoContract,
    load_repo_contract,
)
from dorf.runtime import (
    Assignment,
    Job,
    JobBinding,
    JobConversation,
    Room,
    Worker,
    WorkerAgentTurn,
)

ADMISSION_REVIEW_TIMEOUT_SECONDS = 180
ADMISSION_WORKSPACE = "/workspace/admission"
REQUIRED_GITHUB_APP_PERMISSIONS = {
    "contents": "write",
    "issues": "read",
    "metadata": "read",
    "pull_requests": "write",
}
GITHUB_PERMISSION_LEVELS = {"read": 1, "write": 2, "admin": 3}
ADMISSION_CHECKS = (
    "exact-repository-and-issue",
    "github-app-workflow-authority",
    "official-image",
    "repository-prepare-and-smoke",
    "github-dry-run-push",
    "scoped-provider-route",
    "bounded-implementation-turn",
    "bounded-codex-reviewer-turn",
)


@dataclass(frozen=True)
class AdmissionFailure:
    code: str
    owner: str
    summary: str
    repair: str
    consequence: str
    automatic_continuation: bool

    def record(self) -> dict[str, object]:
        return {
            "automatic_continuation": self.automatic_continuation,
            "code": self.code,
            "consequence": self.consequence,
            "owner": self.owner,
            "repair": self.repair,
            "summary": self.summary,
        }


@dataclass(frozen=True)
class CodingAdmissionRequest:
    repo_path: str
    target_branch: str
    issue_number: int | None
    provider_connection: str | None = None
    model: str | None = None
    reasoning_effort: str | None = None


@dataclass(frozen=True)
class CodingAdmissionProof:
    proof_id: str
    repository: str
    issue: GitHubIssue | None
    target_branch: str
    target_start_sha: str
    image_fingerprint: str
    provider_connection: str
    reviewer: str
    contract: RepoContract = field(repr=False, compare=False)
    codex_config: CodexConfig = field(repr=False, compare=False)
    git_author: GitAuthorIdentity = field(repr=False, compare=False)
    environment_config: IncusConfig = field(repr=False, compare=False)
    installation_token: str = field(repr=False, compare=False)

    @classmethod
    def create(
        cls,
        *,
        repository: str,
        issue: GitHubIssue | None,
        target_branch: str,
        target_start_sha: str,
        image_fingerprint: str,
        provider_connection: str,
        reviewer: str,
        contract: RepoContract,
        codex_config: CodexConfig,
        git_author: GitAuthorIdentity,
        environment_config: IncusConfig,
        installation_token: str,
    ) -> CodingAdmissionProof:
        facts = {
            "checks": ADMISSION_CHECKS,
            "image_fingerprint": image_fingerprint,
            "issue_number": issue.number if issue is not None else None,
            "provider_connection": provider_connection,
            "repository": repository,
            "reviewer": reviewer,
            "target_branch": target_branch,
            "target_start_sha": target_start_sha,
        }
        proof_id = "admission-" + hashlib.sha256(
            json.dumps(facts, sort_keys=True, separators=(",", ":")).encode()
        ).hexdigest()[:24]
        return cls(
            proof_id=proof_id,
            repository=repository,
            issue=issue,
            target_branch=target_branch,
            target_start_sha=target_start_sha,
            image_fingerprint=image_fingerprint,
            provider_connection=provider_connection,
            reviewer=reviewer,
            contract=contract,
            codex_config=codex_config,
            git_author=git_author,
            environment_config=environment_config,
            installation_token=installation_token,
        )

    def record(self) -> dict[str, object]:
        return {
            "checks": list(ADMISSION_CHECKS),
            "image_fingerprint": self.image_fingerprint,
            "issue_number": self.issue.number if self.issue is not None else None,
            "proof_id": self.proof_id,
            "provider_connection": self.provider_connection,
            "repository": self.repository,
            "reviewer": self.reviewer,
            "target_branch": self.target_branch,
            "target_start_sha": self.target_start_sha,
        }


@dataclass(frozen=True)
class CodingAdmissionResult:
    proof: CodingAdmissionProof | None = None
    failures: tuple[AdmissionFailure, ...] = ()

    @property
    def ready(self) -> bool:
        return self.proof is not None and not self.failures


class CodingAdmissionBackend(Protocol):
    def check_repository(
        self, request: CodingAdmissionRequest
    ) -> tuple[AdmissionFailure, ...]: ...

    def check_github(self, request: CodingAdmissionRequest) -> tuple[AdmissionFailure, ...]: ...

    def check_platform(self, request: CodingAdmissionRequest) -> tuple[AdmissionFailure, ...]: ...

    def exercise_consumer_path(
        self, request: CodingAdmissionRequest
    ) -> tuple[AdmissionFailure, ...]: ...

    def proof(self, request: CodingAdmissionRequest) -> CodingAdmissionProof: ...


class CodingAdmissionPreflight:
    """Aggregate discovery, then exercise the exact disposable consumer path once."""

    def __init__(self, backend: CodingAdmissionBackend | None = None) -> None:
        self.backend = backend or LocalCodingAdmissionBackend()

    def prove(self, request: CodingAdmissionRequest) -> CodingAdmissionResult:
        failures: list[AdmissionFailure] = []
        for check in (
            self.backend.check_repository,
            self.backend.check_github,
            self.backend.check_platform,
        ):
            failures.extend(check(request))
        if failures:
            return CodingAdmissionResult(failures=tuple(failures))
        failures.extend(self.backend.exercise_consumer_path(request))
        if failures:
            return CodingAdmissionResult(failures=tuple(failures))
        return CodingAdmissionResult(proof=self.backend.proof(request))


class LocalCodingAdmissionBackend:
    """Concrete host checks and one disposable Incus coding workstation proof."""

    def __init__(
        self,
        *,
        probe: IncusRunnerProbe | None = None,
        gateway: ProviderGateway | None = None,
    ) -> None:
        self.probe = probe or IncusRunnerProbe()
        self.gateway = gateway
        self.repo: Path | None = None
        self.repository: str | None = None
        self.contract: RepoContract | None = None
        self.codex_config: CodexConfig | None = None
        self.git_author: GitAuthorIdentity | None = None
        self.profile = None
        self.environment_config: IncusConfig | None = None
        self.provider_connection: str | None = None
        self.image_fingerprint: str | None = None
        self.installation_token: str | None = None
        self.issue: GitHubIssue | None = None
        self.target_start_sha: str | None = None
        self.reviewer = "codex"

    def check_repository(
        self, request: CodingAdmissionRequest
    ) -> tuple[AdmissionFailure, ...]:
        failures: list[AdmissionFailure] = []
        self.repo = Path(request.repo_path).resolve()
        status = _git(self.repo, "status", "--porcelain")
        if status.returncode != 0:
            failures.append(
                _failure(
                    "repository-unavailable",
                    "repository owner",
                    _message(status),
                    "Run this delegation from a usable Git checkout.",
                    "The exact source repository cannot be prepared.",
                )
            )
        elif status.stdout.strip():
            failures.append(
                _failure(
                    "repository-dirty",
                    "repository owner",
                    "Target repository has uncommitted changes.",
                    "Commit or stash the target repository changes, then repeat the delegation.",
                    "Dorf cannot pin an unambiguous target start state.",
                )
            )
        remote = _git(self.repo, "remote", "get-url", "origin")
        repository = _parse_github_repository(remote.stdout.strip())
        if remote.returncode != 0 or not repository:
            failures.append(
                _failure(
                    "repository-origin",
                    "repository owner",
                    _message(remote) if remote.returncode else "origin is not a GitHub repository",
                    "Configure origin as the exact GitHub repository to delegate.",
                    "GitHub issue and write authority cannot be scoped to this checkout.",
                )
            )
        else:
            self.repository = repository
        try:
            self.contract = load_repo_contract(self.repo)
        except (ContractValidationError, OSError) as error:
            failures.append(
                _failure(
                    "repository-contract",
                    "repository owner",
                    str(error),
                    "Repair .dorf.toml and commit the repository contract.",
                    "Repository preparation and reviewer policy are unknown.",
                )
            )
        if self.contract is not None:
            try:
                self.codex_config = resolve_codex_config(
                    self.contract.primary_codex,
                    model=request.model,
                    reasoning_effort=request.reasoning_effort,
                )
            except AgentConfigValidationError as error:
                failures.append(
                    _failure(
                        "implementation-codex-config",
                        "repository owner",
                        str(error),
                        "Repair [agent.codex] or the model override.",
                        "The implementation harness cannot start with the selected model.",
                    )
                )
            reviewer = (
                self.contract.review.agents.get(self.reviewer)
                if self.contract.review is not None
                else None
            )
            if reviewer is None or not reviewer.enabled:
                failures.append(
                    _failure(
                        "codex-reviewer-config",
                        "repository owner",
                        "No enabled review.agents.codex command is configured.",
                        "Configure and enable review.agents.codex in .dorf.toml.",
                        "AFK verification has no configured Codex reviewer path.",
                    )
                )
            elif REVIEW_PROMPT_PLACEHOLDER not in reviewer.command:
                failures.append(
                    _failure(
                        "codex-reviewer-prompt",
                        "repository owner",
                        f"review.agents.codex.command lacks {REVIEW_PROMPT_PLACEHOLDER}.",
                        f"Add {REVIEW_PROMPT_PLACEHOLDER} to review.agents.codex.command.",
                        "The bounded proof cannot send its real reviewer turn.",
                    )
                )
            elif _command_prefix(reviewer.command) != ["codex", "exec"]:
                failures.append(
                    _failure(
                        "codex-reviewer-command",
                        "repository owner",
                        "review.agents.codex.command does not invoke `codex exec`.",
                        "Configure review.agents.codex.command to invoke `codex exec`.",
                        "Reviewer binary, model, and credential execution are unproved.",
                    )
                )
        name = _git(self.repo, "config", "user.name")
        email = _git(self.repo, "config", "user.email")
        if (
            name.returncode
            or email.returncode
            or not name.stdout.strip()
            or not email.stdout.strip()
        ):
            failures.append(
                _failure(
                    "git-author",
                    "repository owner",
                    "Repository Git author name or email is not configured.",
                    "Set git user.name and user.email in this repository.",
                    "The admitted Job cannot create attributable commits.",
                )
            )
        else:
            self.git_author = GitAuthorIdentity(name.stdout.strip(), email.stdout.strip())
        return tuple(failures)

    def check_github(self, request: CodingAdmissionRequest) -> tuple[AdmissionFailure, ...]:
        if self.repository is None:
            return ()
        failures: list[AdmissionFailure] = []
        try:
            config = load_github_app_config()
            minted = GitHubAppTokenClient().mint_installation_token(config)
            if not isinstance(minted, GitHubInstallationToken):
                minted = GitHubInstallationToken(str(minted))
            self.installation_token = minted.token
            missing = _missing_app_permissions(minted.permissions)
            if missing:
                failures.append(
                    _failure(
                        "github-app-permissions",
                        "GitHub App owner",
                        "GitHub App token lacks workflow permissions: " + ", ".join(missing),
                        "Update and reinstall the Dorf GitHub App with metadata read, issues read, "
                        "contents write, and pull requests write.",
                        "The coding workflow cannot create its branch and PR or read its issue.",
                    )
                )
            client = GitHubRepositoryClient(minted.token)
            repository_permissions = client.get_repository_permissions(self.repository)
            if repository_permissions.get("push") is not True:
                failures.append(
                    _failure(
                        "github-repository-write",
                        "GitHub repository owner",
                        f"The GitHub App cannot push to {self.repository}.",
                        "Grant the Dorf GitHub App write access to this exact repository.",
                        "The coding Job cannot publish its branch.",
                    )
                )
            self.target_start_sha = client.get_branch_sha(
                self.repository, request.target_branch
            )
            assert self.repo is not None
            local_head = _git(self.repo, "rev-parse", "HEAD")
            if (
                local_head.returncode != 0
                or local_head.stdout.strip() != self.target_start_sha
            ):
                failures.append(
                    _failure(
                        "target-branch-diverged",
                        "repository owner",
                        "Local HEAD does not equal the exact GitHub target branch head.",
                        f"Update local {request.target_branch} to the GitHub branch head, "
                        "then retry.",
                        "The delegated goal cannot be pinned to one exact starting commit.",
                    )
                )
            if request.issue_number is not None:
                self.issue = client.get_issue(self.repository, request.issue_number)
        except (GitHubAppConfigError, GitHubAppVerificationError, GitHubRepositoryError) as error:
            failures.append(
                _failure(
                    "github-authority",
                    "GitHub App owner",
                    str(error),
                    "Run `dorf github setup` and grant the App this repository's required access.",
                    "The exact repository, issue, branch, and publication authority are unproved.",
                )
            )
        return tuple(failures)

    def check_platform(self, request: CodingAdmissionRequest) -> tuple[AdmissionFailure, ...]:
        failures: list[AdmissionFailure] = []
        try:
            self.profile = load_deployment_profile()
            self.provider_connection = (
                request.provider_connection or self.profile.provider_connection
            )
            self.image_fingerprint = self.profile.image_fingerprint
            if self.image_fingerprint is None:
                failures.append(
                    _failure(
                        "official-image-unpinned",
                        "Dorf setup owner",
                        "The deployment profile does not record a promoted image fingerprint.",
                        "Run `dorf setup` to install and pin the promoted official image.",
                        "Admission cannot bind proof and execution to one official image.",
                    )
                )
            repository_incus = (
                self.contract.incus_config if self.contract is not None else {}
            )
            self.environment_config = IncusConfig(
                template=self.image_fingerprint or self.profile.incus.template,
                network=repository_incus.get("network", self.profile.incus.network),
                root_disk_size=repository_incus.get(
                    "root_disk_size", self.profile.incus.root_disk_size
                ),
            )
        except DeploymentProfileError as error:
            failures.append(
                _failure(
                    "deployment-profile",
                    "Dorf setup owner",
                    str(error),
                    "Run `dorf setup` to create a valid deployment profile.",
                    "The official image and implementation provider selection are unknown.",
                )
            )
        if self.environment_config is not None:
            result = IncusDoctor(self.probe).fast_check(self.environment_config)
            for failure in result.failures:
                failures.append(
                    _failure(
                        f"incus-{failure.code}",
                        "Incus owner",
                        failure.message,
                        result.remediation
                        or "Run `dorf doctor` and repair the Incus prerequisite.",
                        "The disposable proof and admitted Room cannot use the selected image.",
                    )
                )
        if self.provider_connection is not None and self.environment_config is not None:
            try:
                gateway = self._gateway()
                gateway.require_connection(self.provider_connection)
            except (ProviderGatewayError, OSError, RuntimeError, ValueError) as error:
                failures.append(
                    _failure(
                        "provider-route",
                        "provider connection owner",
                        str(error),
                        getattr(
                            error,
                            "remediation",
                            "Reconnect the selected Provider Connection.",
                        ),
                        "Implementation and Codex review inference cannot execute.",
                    )
                )
        return tuple(failures)

    def exercise_consumer_path(
        self, request: CodingAdmissionRequest
    ) -> tuple[AdmissionFailure, ...]:
        assert self.repository is not None
        assert self.contract is not None
        assert self.codex_config is not None
        assert self.codex_config.model is not None
        assert self.codex_config.reasoning_effort is not None
        assert self.environment_config is not None
        assert self.provider_connection is not None
        assert self.image_fingerprint is not None
        assert self.installation_token is not None
        assert self.target_start_sha is not None
        vm = f"dorf-coding-admission-{secrets.token_hex(4)}"
        binding = _disposable_job_binding(vm, self.codex_config)
        consumer = f"room:{binding.room.id}"
        route = None
        failures: list[AdmissionFailure] = []
        vm_created = False
        try:
            existing = self.probe.run(["incus", "info", vm], timeout_seconds=30)
            if existing.returncode == 0:
                raise RuntimeError(f"disposable VM name already exists: {vm}")
            if not _incus_instance_absent(existing):
                raise RuntimeError(
                    "could not prove disposable VM name is unused: " + _message(existing)
                )
            init = self.probe.run(
                [
                    "incus",
                    "init",
                    self.image_fingerprint,
                    vm,
                    "--vm",
                    "--network",
                    self.environment_config.network,
                    "-d",
                    f"root,size={self.environment_config.root_disk_size}",
                ],
                timeout_seconds=60,
            )
            if init.returncode:
                raise RuntimeError(_message(init))
            vm_created = True
            start = self.probe.run(["incus", "start", vm], timeout_seconds=60)
            if start.returncode:
                raise RuntimeError(_message(start))
            if not self._wait_for_guest(vm):
                raise RuntimeError("Incus guest agent did not become ready")
            route = self._gateway().create_route(
                self.provider_connection,
                consumer=consumer,
            )
            self._install_route(vm, route.base_url, route.wire_api, route.api_key)
            self._clone_and_prepare(vm, request)
            self._prove_github_write(vm)
            self._prove_implementation(binding)
            self._prove_reviewer(vm)
        except (OSError, RuntimeError, subprocess.TimeoutExpired, ProviderGatewayError) as error:
            failures.append(
                _failure(
                    "consumer-path",
                    "coding environment owner",
                    str(error),
                    "Repair the reported repository, service, GitHub, provider, or reviewer path, "
                    "then repeat the same delegation.",
                    "A metadata-only check could pass while real coding execution fails.",
                )
            )
        finally:
            try:
                if route is not None:
                    try:
                        if not self._gateway().revoke_route(route.id):
                            raise RuntimeError(
                                "temporary provider route was not found during cleanup"
                            )
                        if self._gateway().route_for_consumer(consumer) is not None:
                            raise RuntimeError("temporary provider route remains after revocation")
                    except (OSError, ProviderGatewayError, RuntimeError) as error:
                        failures.append(
                            _failure(
                                "provider-route-cleanup",
                                "provider connection owner",
                                str(error),
                                "Run `dorf doctor`, revoke the coding-admission route, and retry.",
                                "Admission is blocked because a scoped route may have leaked.",
                            )
                        )
            finally:
                if vm_created:
                    info = self.probe.run(["incus", "info", vm], timeout_seconds=30)
                    if info.returncode == 0:
                        deleted = self.probe.run(
                            ["incus", "delete", vm, "--force"], timeout_seconds=60
                        )
                    elif _incus_instance_absent(info):
                        deleted = None
                    else:
                        deleted = info
                    if deleted is not None and deleted.returncode:
                        failures.append(
                            _failure(
                                "disposable-room-cleanup",
                                "Incus owner",
                                _message(deleted),
                                f"Delete disposable VM {vm} with "
                                f"`incus delete {vm} --force`, then retry.",
                                "Admission is blocked because its disposable Room remains.",
                            )
                        )
        return tuple(failures)

    def proof(self, request: CodingAdmissionRequest) -> CodingAdmissionProof:
        if not all(
            (
                self.repository,
                self.contract,
                self.codex_config,
                self.git_author,
                self.environment_config,
                self.provider_connection,
                self.image_fingerprint,
                self.installation_token,
                self.target_start_sha,
            )
        ):
            raise RuntimeError("coding admission proof is incomplete")
        return CodingAdmissionProof.create(
            repository=self.repository,
            issue=self.issue,
            target_branch=request.target_branch,
            target_start_sha=self.target_start_sha,
            image_fingerprint=self.image_fingerprint,
            provider_connection=self.provider_connection,
            reviewer=self.reviewer,
            contract=self.contract,
            codex_config=self.codex_config,
            git_author=self.git_author,
            environment_config=self.environment_config,
            installation_token=self.installation_token,
        )

    def _gateway(self) -> ProviderGateway:
        if self.gateway is None:
            assert self.environment_config is not None
            from dorf.adapters.environments import incus_bridge_ipv4

            self.gateway = ProviderGateway.open(
                bind_address=incus_bridge_ipv4(
                    self.environment_config.network,
                    probe=self.probe,
                )
            )
        return self.gateway

    def _wait_for_guest(self, vm: str) -> bool:
        for attempt in range(30):
            result = self.probe.run(
                ["incus", "exec", vm, "--", "true"],
                timeout_seconds=10,
            )
            if result.returncode == 0:
                return True
            if attempt < 29:
                time.sleep(1)
        return False

    def _install_route(self, vm: str, base_url: str, wire_api: str, api_key: str) -> None:
        config = "\n".join(
            (
                'model_provider = "dorf"',
                "",
                "[model_providers.dorf]",
                'name = "Dorf Provider Gateway"',
                f"base_url = {json.dumps(base_url)}",
                f"env_key = {json.dumps(CODEX_ROUTE_ENV_KEY)}",
                f"wire_api = {json.dumps(wire_api)}",
                "requires_openai_auth = false",
                "",
            )
        )
        for path, directory, content in (
            (CODEX_CONFIG_PATH, "/root/.codex", config),
            (CODEX_ROUTE_CREDENTIAL_PATH, "/root/.config/dorf", f"{api_key}\n"),
        ):
            result = self.probe.run(
                [
                    "incus",
                    "exec",
                    vm,
                    "--",
                    "bash",
                    "-lc",
                    f"umask 077; mkdir -p {directory}; cat > {path}",
                ],
                input=content,
                timeout_seconds=30,
            )
            if result.returncode:
                raise RuntimeError("Could not install the disposable provider route")

    def _clone_and_prepare(self, vm: str, request: CodingAdmissionRequest) -> None:
        assert self.repository is not None
        assert self.contract is not None
        assert self.installation_token is not None
        clone = git_clone_workspace_script(
            f"https://github.com/{self.repository}.git",
            request.target_branch,
            ADMISSION_WORKSPACE,
        )
        result = self.probe.run(
            [
                "incus",
                "exec",
                vm,
                "--",
                "bash",
                "-lc",
                f"mkdir -p {ADMISSION_WORKSPACE}; {clone}",
            ],
            input=f"{self.installation_token}\n",
            timeout_seconds=600,
        )
        if result.returncode:
            raise RuntimeError(
                "repository clone failed: "
                + _redact(_message(result), self.installation_token)
            )
        for kind in ("prepare", "smoke"):
            command = self.contract.commands.get(kind)
            if command is None:
                continue
            result = self.probe.run(
                [
                    "incus",
                    "exec",
                    vm,
                    "--cwd",
                    ADMISSION_WORKSPACE,
                    "--",
                    "env",
                    *[
                        f"{name}={value}"
                        for name, value in self._command_env(request).items()
                    ],
                    "bash",
                    "-lc",
                    command,
                ],
                timeout_seconds=600,
            )
            if result.returncode:
                raise RuntimeError(f"repository {kind} failed: {_message(result)}")

    def _prove_github_write(self, vm: str) -> None:
        branch = f"refs/heads/dorf-admission-proof-{secrets.token_hex(4)}"
        result = self.probe.run(
            [
                "incus",
                "exec",
                vm,
                "--cwd",
                ADMISSION_WORKSPACE,
                "--",
                "env",
                "GIT_TERMINAL_PROMPT=0",
                "git",
                "push",
                "--dry-run",
                "origin",
                f"HEAD:{branch}",
            ],
            timeout_seconds=60,
        )
        if result.returncode:
            raise RuntimeError("GitHub dry-run push failed: " + _message(result))

    def _prove_reviewer(self, vm: str) -> None:
        assert self.contract is not None
        assert self.contract.review is not None
        reviewer = self.contract.review.agents[self.reviewer]
        nonce = f"DORF_ADMISSION_READY_{secrets.token_hex(8)}"
        command = reviewer.command.replace(
            REVIEW_PROMPT_PLACEHOLDER,
            shlex.quote(f"Reply with exactly: {nonce}"),
        )
        script = (
            f"IFS= read -r {CODEX_ROUTE_ENV_KEY} < {CODEX_ROUTE_CREDENTIAL_PATH}; "
            f"export {CODEX_ROUTE_ENV_KEY}; exec {command}"
        )
        result = self.probe.run(
            [
                "incus",
                "exec",
                vm,
                "--cwd",
                ADMISSION_WORKSPACE,
                "--",
                "bash",
                "-lc",
                script,
            ],
            timeout_seconds=min(
                self.contract.review.timeout_seconds,
                ADMISSION_REVIEW_TIMEOUT_SECONDS,
            ),
        )
        response_lines = [line.strip() for line in result.stdout.splitlines() if line.strip()]
        if result.returncode or not response_lines or response_lines[-1] != nonce:
            raise RuntimeError(
                "bounded Codex reviewer turn failed: "
                + (_message(result) if result.returncode else "expected response was absent")
            )

    def _prove_implementation(self, binding: JobBinding) -> None:
        assert self.codex_config is not None
        assert self.codex_config.model is not None
        assert self.codex_config.reasoning_effort is not None
        assert self.environment_config is not None
        assert self.provider_connection is not None
        nonce = f"DORF_IMPLEMENTATION_READY_{secrets.token_hex(8)}"
        environment = CodexRoomEnvironment(
            IncusEnvironment(self.environment_config, probe=self.probe),
            self._gateway(),
            connection_name=self.provider_connection,
        )
        driver = CodexDriver(environment)
        driver.prepare(binding)
        with tempfile.TemporaryDirectory(prefix="dorf-coding-admission-") as output_dir:
            outcome = driver.start_job_conversation(
                binding,
                WorkerAgentTurn(
                    f"Reply with exactly: {nonce}",
                    Path(output_dir) / "implementation.log",
                    self.codex_config.model,
                    self.codex_config.reasoning_effort,
                ),
                conversation_started=lambda thread_id: None,
                turn_prepared=lambda baseline: None,
                turn_started=lambda turn_id: None,
                timeout_seconds=ADMISSION_REVIEW_TIMEOUT_SECONDS,
            )
        if outcome.status != "completed":
            raise RuntimeError(
                f"bounded implementation app-server turn ended with status {outcome.status}"
            )

    def _command_env(self, request: CodingAdmissionRequest) -> dict[str, str]:
        assert self.contract is not None
        assert self.repo is not None
        assert self.repository is not None
        assert self.target_start_sha is not None
        values = {
            "DORF_ADMISSION_PROOF": "1",
            "DORF_ASSIGNMENT_ID": "disposable-admission",
            "DORF_JOB_BRANCH": "dorf/disposable-admission",
            "DORF_JOB_NAME": "disposable-admission",
            "DORF_READY_PATH": "/tmp/dorf/ready.json",
            "DORF_TARGET_BRANCH": request.target_branch,
            "DORF_TARGET_START_SHA": self.target_start_sha,
            "DORF_WORKER_NAME": "disposable-admission",
            "DORF_WORKSPACE": ADMISSION_WORKSPACE,
        }
        sources = {
            "dorf.assignment_id": "disposable-admission",
            "dorf.job_branch": "dorf/disposable-admission",
            "dorf.job_name": "disposable-admission",
            "dorf.ready_path": "/tmp/dorf/ready.json",
            "dorf.target_branch": request.target_branch,
            "dorf.target_repo": str(self.repo),
            "dorf.target_start_sha": self.target_start_sha,
            "dorf.worker_name": "disposable-admission",
            "dorf.workspace": ADMISSION_WORKSPACE,
        }
        for name, source in self.contract.env.items():
            if source.startswith("host."):
                host_name = source.removeprefix("host.")
                if host_name in os.environ:
                    values[name] = os.environ[host_name]
                continue
            if source not in sources:
                raise RuntimeError(f"Unsupported env source for {name}: {source}")
            values[name] = sources[source]
        return values


def _failure(
    code: str,
    owner: str,
    summary: str,
    repair: str,
    consequence: str,
) -> AdmissionFailure:
    return AdmissionFailure(code, owner, summary, repair, consequence, False)


def _disposable_job_binding(vm: str, codex_config: CodexConfig) -> JobBinding:
    worker_name = "coding-admission"
    job_name = "disposable-admission"
    room_id = f"admission-{vm}"
    timestamp = "disposable"
    return JobBinding(
        Job(
            id=0,
            name=job_name,
            status="open",
            goal_version=1,
            goal="Prove the disposable coding implementation path.",
            created_at=timestamp,
            updated_at=timestamp,
        ),
        Assignment(
            id="disposable-admission",
            job_name=job_name,
            worker_name=worker_name,
            generation=1,
            status="open",
            room_id=room_id,
            workspace=ADMISSION_WORKSPACE,
            started_at=timestamp,
            ended_at=None,
        ),
        JobConversation(
            id="disposable-admission",
            job_name=job_name,
            native_conversation_id=None,
            model=codex_config.model or "",
            reasoning_effort=codex_config.reasoning_effort or "",
            status="idle",
            error=None,
            created_at=timestamp,
            updated_at=timestamp,
        ),
        Worker(
            id=0,
            name=worker_name,
            harness_type="codex",
            provenance="coding-workflow",
            lifecycle_policy="disposable",
            status="ready",
            error=None,
            current_room_id=room_id,
            general_conversation_id=None,
            created_at=timestamp,
            updated_at=timestamp,
        ),
        Room(
            id=room_id,
            worker_name=worker_name,
            room_type=IncusEnvironment.environment_type,
            provider_id=vm,
            workspace="/workspace",
            status="ready",
            error=None,
            metadata={},
            created_at=timestamp,
            updated_at=timestamp,
        ),
    )


def _git(repo: Path, *args: str) -> subprocess.CompletedProcess[str]:
    try:
        return subprocess.run(
            ["git", *args],
            cwd=repo,
            text=True,
            capture_output=True,
            check=False,
        )
    except OSError as error:
        return subprocess.CompletedProcess(["git", *args], 127, "", str(error))


def _message(result: subprocess.CompletedProcess[str]) -> str:
    return result.stderr.strip() or result.stdout.strip() or "command failed"


def _redact(message: str, secret: str) -> str:
    return message.replace(secret, "<redacted>").replace(
        urllib.parse.quote(secret, safe=""), "<redacted>"
    )


def _command_prefix(command: str) -> list[str]:
    try:
        return shlex.split(command)[:2]
    except ValueError:
        return []


def _incus_instance_absent(result: subprocess.CompletedProcess[str]) -> bool:
    message = _message(result).lower()
    return any(marker in message for marker in ("not found", "does not exist", "doesn't exist"))


def _parse_github_repository(origin: str) -> str | None:
    match = re.match(
        r"^(?:https://github\.com/|git@github\.com:|ssh://git@github\.com/)"
        r"([^/]+)/([^/]+?)(?:\.git)?/?$",
        origin,
    )
    return f"{match.group(1)}/{match.group(2)}" if match else None


def _missing_app_permissions(permissions: dict[str, str] | None) -> list[str]:
    if permissions is None:
        return [f"{name}:{level}" for name, level in REQUIRED_GITHUB_APP_PERMISSIONS.items()]
    return [
        f"{name}:{level}"
        for name, level in REQUIRED_GITHUB_APP_PERMISSIONS.items()
        if GITHUB_PERMISSION_LEVELS.get(permissions.get(name, ""), 0)
        < GITHUB_PERMISSION_LEVELS[level]
    ]
