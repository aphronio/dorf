"""One no-durable-mutation proof for coding-workflow admission."""

from __future__ import annotations

import hashlib
import json
import os
import re
import secrets
import subprocess
import tempfile
import time
import urllib.parse
from dataclasses import dataclass, field
from pathlib import Path
from typing import Protocol

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
from dorf.repo_contract import ContractValidationError, RepoContract, load_repo_contract
from dorf.runtime import (
    Assignment,
    Job,
    JobBinding,
    JobConversation,
    Room,
    Worker,
)
from dorf.sdk import (
    AgentConfigValidationError,
    CodexConfig,
    Dorf,
    IncusConfig,
    RoomRouteGateway,
    resolve_codex_config,
)
from dorf.workflows.shadow_verifier import (
    NO_FINDINGS,
    deepseek_command,
    deepseek_extension,
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
    "bounded-deepseek-diff-turn",
)


@dataclass(frozen=True)
class GitHubAuthorityApproval:
    repository: str
    installation_id: str
    missing_authority: str
    why_needed: str
    action: str
    scope: str
    approve_consequence: str
    decline_consequence: str
    automatic_resume: str
    url: str

    def record(self) -> dict[str, str]:
        return {
            "action": self.action,
            "approve_consequence": self.approve_consequence,
            "automatic_resume": self.automatic_resume,
            "decline_consequence": self.decline_consequence,
            "installation_id": self.installation_id,
            "missing_authority": self.missing_authority,
            "repository": self.repository,
            "scope": self.scope,
            "url": self.url,
            "why_needed": self.why_needed,
        }


@dataclass(frozen=True)
class AdmissionFailure:
    code: str
    owner: str
    summary: str
    repair: str
    consequence: str
    automatic_continuation: bool
    approval: GitHubAuthorityApproval | None = None

    def record(self) -> dict[str, object]:
        record: dict[str, object] = {
            "automatic_continuation": self.automatic_continuation,
            "code": self.code,
            "consequence": self.consequence,
            "owner": self.owner,
            "repair": self.repair,
            "summary": self.summary,
        }
        if self.approval is not None:
            record["approval"] = self.approval.record()
        return record


@dataclass(frozen=True)
class CodingAdmissionRequest:
    repo_path: str
    target_branch: str
    issue_number: int | None
    command: str = "issue"
    target_start_sha: str | None = None
    repository: str | None = None
    installation_id: str | None = None
    provider_connection: str | None = None
    model: str | None = None
    reasoning_effort: str | None = None

    def record(self) -> dict[str, object]:
        return {
            "command": self.command,
            "issue_number": self.issue_number,
            "installation_id": self.installation_id,
            "model": self.model,
            "provider_connection": self.provider_connection,
            "reasoning_effort": self.reasoning_effort,
            "repo_path": str(Path(self.repo_path).resolve()),
            "repository": self.repository,
            "target_branch": self.target_branch,
            "target_start_sha": self.target_start_sha,
        }


@dataclass(frozen=True)
class CodingAdmissionProof:
    proof_id: str
    repository: str
    installation_id: str
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
    approval_attempt_id: str | None = field(default=None, repr=False, compare=False)

    @classmethod
    def create(
        cls,
        *,
        repository: str,
        installation_id: str,
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
            "installation_id": installation_id,
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
            installation_id=installation_id,
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
        record: dict[str, object] = {
            "checks": list(ADMISSION_CHECKS),
            "image_fingerprint": self.image_fingerprint,
            "installation_id": self.installation_id,
            "issue_number": self.issue.number if self.issue is not None else None,
            "proof_id": self.proof_id,
            "provider_connection": self.provider_connection,
            "repository": self.repository,
            "reviewer": self.reviewer,
            "target_branch": self.target_branch,
            "target_start_sha": self.target_start_sha,
        }
        if self.approval_attempt_id is not None:
            record["approval_attempt_id"] = self.approval_attempt_id
        return record


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
        probe=None,
        gateway: RoomRouteGateway | None = None,
    ) -> None:
        self.probe = probe or Dorf.new_environment_probe()
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
        self.installation_id: str | None = None
        self.issue: GitHubIssue | None = None
        self.target_start_sha: str | None = None
        self.reviewer = "deepseek-diff"

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
        if request.target_start_sha is not None:
            current_head = _git(self.repo, "rev-parse", "HEAD")
            if (
                current_head.returncode != 0
                or current_head.stdout.strip() != request.target_start_sha
            ):
                failures.append(
                    _failure(
                        "delegation-start-changed",
                        "repository owner",
                        "Repository HEAD changed after this delegation was pinned.",
                        "Restore the delegated starting commit or make a new delegation.",
                        "Automatic continuation cannot silently change the source baseline.",
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
            if request.repository is not None and repository != request.repository:
                failures.append(
                    _failure(
                        "delegation-repository-changed",
                        "repository owner",
                        "Repository origin changed after this delegation was pinned.",
                        f"Restore origin to {request.repository} or make a new delegation.",
                        "Automatic continuation cannot retarget GitHub authority to another "
                        "repository.",
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
        config = None
        try:
            config = load_github_app_config()
            if (
                request.installation_id is not None
                and config.installation_id != request.installation_id
            ):
                return (
                    _failure(
                        "delegation-installation-changed",
                        "GitHub App installation owner",
                        "Configured GitHub App installation changed after this delegation "
                        "was pinned.",
                        f"Restore GitHub App installation {request.installation_id} or make a "
                        "new delegation.",
                        "A different installation cannot approve or consume this retained "
                        "delegation.",
                    ),
                )
            self.installation_id = config.installation_id
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
        except GitHubRepositoryError as error:
            if (
                error.status_code == 404
                and self.target_start_sha is None
                and config is not None
            ):
                approval = _github_repository_approval(
                    repository=self.repository,
                    target_branch=request.target_branch,
                    issue_number=request.issue_number,
                    installation_id=config.installation_id,
                    app_slug=getattr(config, "app_slug", None),
                )
                failures.append(
                    AdmissionFailure(
                        "github-repository-authority",
                        "GitHub App installation owner",
                        f"The Dorf GitHub App installation cannot access {self.repository}.",
                        approval.action,
                        approval.approve_consequence,
                        True,
                        approval,
                    )
                )
            else:
                failures.append(
                    _failure(
                        "github-authority",
                        "GitHub App owner",
                        str(error),
                        "Run `dorf github setup` and grant the App this repository's required "
                        "access.",
                        "The exact repository, issue, branch, and publication authority are "
                        "unproved.",
                    )
                )
        except (GitHubAppConfigError, GitHubAppVerificationError) as error:
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
            result = Dorf.check_environment(self.environment_config, probe=self.probe)
            for failure in result.failures:
                if (
                    failure.code == "incus-template"
                    and self.image_fingerprint is not None
                    and self.environment_config.template == self.image_fingerprint
                ):
                    failures.append(
                        _failure(
                            "official-image-missing",
                            "Dorf setup owner",
                            failure.message,
                            "Run `dorf setup` to reinstall and repin the promoted official image.",
                            "The disposable proof and admitted Room cannot use the selected image.",
                        )
                    )
                    continue
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
                for connection in (self.provider_connection, "deepseek"):
                    gateway.require_connection(connection)
            except (OSError, RuntimeError, ValueError) as error:
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
                        "Implementation or DeepSeek diff verification cannot execute.",
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
            execution = Dorf.disposable_job_execution(
                binding,
                environment_config=self.environment_config,
                provider_connection=self.provider_connection,
                provider_gateway=self._gateway(),
                environment_probe=self.probe,
            )
            route = self._gateway().route_for_consumer(consumer)
            if route is None:
                raise RuntimeError("temporary provider route was not installed")
            self._clone_and_prepare(execution, request)
            self._prove_github_write(execution)
            self._prove_implementation(execution)
            self._prove_reviewer(execution)
        except (OSError, RuntimeError, subprocess.TimeoutExpired) as error:
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
                    except (OSError, RuntimeError) as error:
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
                self.installation_id,
                self.target_start_sha,
            )
        ):
            raise RuntimeError("coding admission proof is incomplete")
        return CodingAdmissionProof.create(
            repository=self.repository,
            installation_id=self.installation_id,
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

    def _gateway(self) -> RoomRouteGateway:
        if self.gateway is None:
            assert self.environment_config is not None
            self.gateway = Dorf.open_provider_gateway(
                self.environment_config, probe=self.probe
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

    def _clone_and_prepare(self, execution, request: CodingAdmissionRequest) -> None:
        assert self.repository is not None
        assert self.contract is not None
        assert self.installation_token is not None
        clone = git_clone_workspace_script(
            f"https://github.com/{self.repository}.git",
            request.target_branch,
            ADMISSION_WORKSPACE,
        )
        result = execution.execute(
            ["bash", "-lc", f"mkdir -p {ADMISSION_WORKSPACE}; {clone}"],
            cwd="/",
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
            result = execution.execute(
                ["bash", "-lc", command],
                cwd=ADMISSION_WORKSPACE,
                env=self._command_env(request),
                input="",
                timeout_seconds=600,
            )
            if result.returncode:
                raise RuntimeError(f"repository {kind} failed: {_message(result)}")

    def _prove_github_write(self, execution) -> None:
        branch = f"refs/heads/dorf-admission-proof-{secrets.token_hex(4)}"
        result = execution.execute(
            [
                "git",
                "push",
                "--dry-run",
                "origin",
                f"HEAD:{branch}",
            ],
            cwd=ADMISSION_WORKSPACE,
            env={"GIT_TERMINAL_PROMPT": "0"},
            input="",
            timeout_seconds=60,
        )
        if result.returncode:
            raise RuntimeError("GitHub dry-run push failed: " + _message(result))

    def _prove_reviewer(self, execution) -> None:
        gateway = self._gateway()
        consumer = f"reviewer:{execution.binding.room.id}"
        route = gateway.create_route("deepseek", consumer=consumer)
        try:
            protocol = (
                "Review /tmp/dorf-review.diff with read-only tools. "
                f"If it has no actionable findings, reply exactly: {NO_FINDINGS}\n"
            )
            setup = (
                "set -euo pipefail; umask 077; cat >/tmp/dorf-provider.mjs; "
                f"printf %s {json.dumps(protocol)} >/tmp/dorf-protocol.md; "
                ": >/tmp/dorf-review.diff"
            )
            prepared = execution.execute(
                ["bash", "-lc", setup],
                cwd=ADMISSION_WORKSPACE,
                input=deepseek_extension(route.base_url),
                timeout_seconds=60,
            )
            if prepared.returncode:
                raise RuntimeError("could not prepare DeepSeek diff admission proof")
            command = (
                "IFS= read -r DORF_PROVIDER_ROUTE_KEY; "
                f"export DORF_PROVIDER_ROUTE_KEY; {deepseek_command()}"
            )
            result = execution.execute(
                ["bash", "-lc", command],
                cwd=ADMISSION_WORKSPACE,
                input=f"{route.api_key}\n",
                timeout_seconds=ADMISSION_REVIEW_TIMEOUT_SECONDS,
            )
            if result.returncode or result.stdout.strip() != NO_FINDINGS:
                raise RuntimeError("bounded DeepSeek diff reviewer turn failed")
        except BaseException as primary:
            try:
                if not gateway.revoke_route(route.id):
                    raise RuntimeError("DeepSeek admission route was not cleaned up")
            except BaseException as cleanup:
                raise RuntimeError(f"{primary}; route cleanup also failed: {cleanup}") from primary
            raise
        if not gateway.revoke_route(route.id):
            raise RuntimeError("DeepSeek admission route was not cleaned up")

    def _prove_implementation(self, execution) -> None:
        assert self.codex_config is not None
        assert self.codex_config.model is not None
        assert self.codex_config.reasoning_effort is not None
        nonce = f"DORF_IMPLEMENTATION_READY_{secrets.token_hex(8)}"
        with tempfile.TemporaryDirectory(prefix="dorf-coding-admission-") as output_dir:
            outcome = execution.run_agent_turn(
                f"Reply with exactly: {nonce}",
                Path(output_dir) / "implementation.log",
                model=self.codex_config.model,
                reasoning_effort=self.codex_config.reasoning_effort,
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


def _github_repository_approval(
    *,
    repository: str,
    target_branch: str,
    issue_number: int | None,
    installation_id: str,
    app_slug: str | None,
) -> GitHubAuthorityApproval:
    issue = f"issue #{issue_number} and " if issue_number is not None else ""
    installation = (
        f"GitHub App `{app_slug}` installation {installation_id}"
        if app_slug
        else f"Dorf GitHub App installation {installation_id}"
    )
    return GitHubAuthorityApproval(
        repository=repository,
        installation_id=installation_id,
        missing_authority=f"Persistent repository access for {installation} to {repository}",
        why_needed=(
            f"Coding admission must read {issue}{target_branch}, write the Job branch, and manage "
            "its pull request."
        ),
        action=(
            f"In GitHub settings, select {repository} for {installation} and save. This grants "
            "that installation persistent repository-wide access with its configured permissions "
            "until an owner changes or removes the access."
        ),
        scope=(
            f"Persistent repository-wide authority on {repository} for installation "
            f"{installation_id}: metadata read, issues read, contents write, and pull requests "
            "write. It is not limited to this issue, branch, or delegation."
        ),
        approve_consequence=(
            f"{installation} gains those configured permissions across {repository}; Dorf can "
            "use them while that repository access remains installed."
        ),
        decline_consequence=(
            f"{installation} does not gain access to {repository}; this pending delegation ends "
            "without creating a Job, branch, Room, or pull request."
        ),
        automatic_resume=(
            f"Dorf polls only installation {installation_id}. When that installation can read "
            f"{repository}:{target_branch}, Dorf reruns exact readiness and continues this "
            "delegation automatically; a different configured installation cannot approve it."
        ),
        url=f"https://github.com/settings/installations/{installation_id}",
    )


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
            room_type=Dorf.environment_type,
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
