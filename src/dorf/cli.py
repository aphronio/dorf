from __future__ import annotations

import json
import os
import re
import secrets
import shlex
import signal
import subprocess
import sys
import urllib.parse
import webbrowser
from collections.abc import Callable, Iterator
from contextlib import contextmanager
from dataclasses import asdict, dataclass, replace
from datetime import UTC, datetime
from pathlib import Path
from time import monotonic, sleep

import typer
from rich.console import Console
from rich.progress import (
    BarColumn,
    DownloadColumn,
    Progress,
    SpinnerColumn,
    TaskProgressColumn,
    TextColumn,
    TimeRemainingColumn,
    TransferSpeedColumn,
)
from rich.status import Status
from rich.text import Text
from rich.theme import Theme

from dorf import Dorf, __version__
from dorf.adapters.agents.codex_config import (
    AgentConfigValidationError,
    CodexConfig,
    resolve_codex_config,
)
from dorf.adapters.environments import (
    IncusConfig,
    IncusDoctor,
    IncusRunnerProbe,
)
from dorf.coding_workspace import (
    GitAuthorIdentity,
    coding_job_goal,
    prepare_git_workspace,
    reset_git_workspace,
)
from dorf.command_runner import (
    CommandInterrupted,
    argv_command,
    shell_command,
)
from dorf.core_setup import CoreSetup, CoreSetupFailed, CoreSetupPaused
from dorf.deployment_profile import (
    DeploymentProfile,
    DeploymentProfileError,
    load_optional_deployment_profile,
    set_default_provider_connection,
)
from dorf.github_app import (
    GitHubAppConfigError,
    GitHubAppManifestFlow,
    GitHubAppManifestFlowError,
    GitHubAppTokenClient,
    GitHubAppVerificationError,
    GitHubInstallationToken,
    GitHubIssue,
    GitHubRepositoryClient,
    GitHubRepositoryError,
    github_app_paths,
    load_github_app_config,
    private_key_permissions_are_locked_down,
)
from dorf.host_setup import (
    HostSetupError,
    IncusHostState,
    host_os_label,
    initialize_pristine_incus,
    inspect_incus_host,
    install_incus_on_arch,
    install_incus_on_ubuntu_2404,
    repair_incus_host,
    supported_incus_host_recipe,
)
from dorf.provider_gateway import (
    DeviceAuthorization,
    ProviderConnection,
    ProviderGateway,
    ProviderGatewayError,
)
from dorf.repo_contract import (
    CONTRACT_FILENAME,
    ContractValidationError,
    RepoContract,
    load_repo_contract,
)
from dorf.runtime import (
    AssignedJobWaitResult,
    InvalidJobNameError,
    InvalidWorkerNameError,
    JobArtifact,
    JobBinding,
    JobInspection,
    JobUnsettledError,
    TimelineEvent,
    WorkerAlreadyAttachedError,
    WorkerBinding,
    WorkerInspection,
    WorkerOfflineError,
    WorkerUnsettledError,
    WorkerWaitResult,
)
from dorf.sdk import (
    DedicatedWorkerCleanupError,
    DorfResourceNotFoundError,
    EnvironmentPrerequisitesError,
    UnsupportedRoomTypeError,
)
from dorf.setup_diagnostics import (
    SetupDiagnostic,
    sanitize_setup_diagnostic,
    write_setup_diagnostic,
)
from dorf.workflows import (
    CodingAdmissionPreflight,
    CodingAdmissionProof,
    CodingAdmissionRequest,
    CodingJob,
    CodingJobPulse,
    CodingStore,
    CodingWorkflow,
    PendingCodingAdmission,
    WorkflowFailure,
    WorkflowOutcome,
    build_coding_job_pulse,
    build_proof_dossier,
    compile_acceptance_checklist,
    prepare_coding_repository,
    render_proof_dossier,
    run_coding_job_command,
)
from dorf.workflows.coding import proof_dossier_commit, review_command_with_dorf_protocol
from dorf.workflows.shadow_verifier import run_shadow_review

app = typer.Typer(help="Manage durable Workers and Jobs in isolated Rooms.")
worker_app = typer.Typer(help="Manage durable Workers and their current Rooms.")
job_app = typer.Typer(help="Manage goal-backed Jobs and their Assignments.")
job_artifact_app = typer.Typer(help="List and export retained Job artifacts.")
github_app = typer.Typer(help="Manage Dorf GitHub App credentials.")
provider_app = typer.Typer(help="Manage shared model-provider connections.")
app.add_typer(worker_app, name="worker")
app.add_typer(job_app, name="job")
job_app.add_typer(job_artifact_app, name="artifact")
app.add_typer(github_app, name="github")
app.add_typer(provider_app, name="provider")
COMMAND_ARGV_ARGUMENT = typer.Argument(..., help="Command argv to run after --.")
REVIEW_AGENT_OPTION = typer.Option(
    None,
    "--agent",
    help="Review agent to run. Repeat to run more than one.",
)
ARTIFACT_DESTINATION_OPTION = typer.Option(
    ...,
    "--to",
    help="Existing destination directory. The recorded artifact filename is preserved.",
)
ARTIFACT_OVERWRITE_OPTION = typer.Option(
    False,
    "--overwrite",
    help="Explicitly replace an existing destination file.",
)
ACCEPTANCE_FILE_OPTION = typer.Option(
    None,
    "--from-file",
    help="Replace the draft goal criteria from a reviewed Markdown checklist.",
)
DORF_BRANCH_PREFIX = "dorf/"
PROTECTED_BRANCHES = frozenset({"main", "master", "trunk"})
DORF_PRIMARY_MOSS = "#5B6B36"
DORF_BRIGHT_MOSS = "#918A4A"
DORF_BORDER = "#465B33"
DORF_MAIN_TEXT = "#CEB87D"
DORF_MUTED_TEXT = "#B8A369"
DORF_ACTIVE = "#F6BD4B"
DORF_ACTIVE_RGB = (246, 189, 75)
GITHUB_AUTHORITY_APPROVAL_TTL_SECONDS = 3600
GITHUB_AUTHORITY_POLL_SECONDS = 10
DORF_THEME = Theme(
    {
        "progress.download": DORF_MAIN_TEXT,
        "progress.percentage": DORF_ACTIVE,
        "progress.data.speed": DORF_MUTED_TEXT,
        "progress.remaining": DORF_MUTED_TEXT,
    }
)


class _ImageDownloadProgress:
    """Render a live terminal download and stable milestones in logs."""

    def __init__(
        self,
        *,
        tty: bool | None = None,
        console: Console | None = None,
    ) -> None:
        self._console = console or Console(
            file=sys.stdout,
            force_terminal=tty,
            theme=DORF_THEME,
        )
        self._tty = self._console.is_terminal if tty is None else tty
        self._progress: Progress | None = None
        self._task_id: int | None = None
        self._last_milestone = 0

    def update(self, downloaded: int, total: int) -> None:
        if total <= 0:
            return
        downloaded = min(max(0, downloaded), total)
        percent = downloaded * 100 // total
        if self._tty:
            if self._progress is None:
                self._progress = Progress(
                    SpinnerColumn(style=DORF_ACTIVE),
                    TextColumn(f"[bold {DORF_ACTIVE}]{{task.description}}[/]"),
                    BarColumn(
                        style=DORF_BORDER,
                        complete_style=DORF_ACTIVE,
                        finished_style=DORF_PRIMARY_MOSS,
                        pulse_style=DORF_ACTIVE,
                    ),
                    TaskProgressColumn(),
                    DownloadColumn(),
                    TransferSpeedColumn(),
                    TimeRemainingColumn(),
                    console=self._console,
                    expand=True,
                    refresh_per_second=12,
                )
                self._progress.start()
                self._task_id = self._progress.add_task(
                    "Downloading Room image",
                    total=total,
                )
            assert self._task_id is not None
            self._progress.update(
                self._task_id,
                completed=downloaded,
                total=total,
                refresh=True,
            )
            if downloaded == total:
                self.finish()
            return

        milestone = min(100, percent // 25 * 25)
        if milestone >= 25 and milestone > self._last_milestone:
            typer.echo(
                f"  Downloading · {milestone}% · {_format_download_size(downloaded)}"
                f" / {_format_download_size(total)}"
            )
            self._last_milestone = milestone

    def finish(self) -> None:
        if self._progress is not None:
            self._progress.stop()
            self._progress = None
            self._task_id = None


class _SetupDisplay:
    """Render calm live setup activity while preserving stable redirected output."""

    _HEADINGS = frozenset(
        {
            "Checking this machine",
            "Room image",
            "AI model provider",
            "Next:",
        }
    )
    _ACTIVITY_MESSAGES = frozenset(
        {
            "Checking the latest immutable Dorf Room image",
            "Importing Dorf Room image into Incus",
        }
    )

    def __init__(
        self,
        *,
        tty: bool | None = None,
        console: Console | None = None,
        plain_emit: Callable[[str], None] = typer.echo,
    ) -> None:
        self._console = console or Console(
            file=sys.stdout,
            force_terminal=tty,
            theme=DORF_THEME,
        )
        self._tty = self._console.is_terminal if tty is None else tty
        self._plain_emit = plain_emit
        self._status: Status | None = None
        self._image_progress = _ImageDownloadProgress(
            tty=self._tty,
            console=self._console,
        )

    def emit(self, message: str) -> None:
        if not self._tty:
            self._plain_emit(message)
            return

        if message == "":
            self._stop_activity()
            self._console.print()
            return
        if message == "◆ Dorf":
            self._console.print(Text(message, style=f"bold {DORF_MAIN_TEXT}"))
            return
        if message == "  Durable workers in private local Rooms":
            self._console.print(Text(message, style=DORF_MUTED_TEXT))
            return
        if message == "Dorf is ready.":
            self._stop_activity()
            self._console.print(Text(message, style=f"bold {DORF_BRIGHT_MOSS}"))
            return
        if message in self._HEADINGS:
            self._stop_activity()
            self._console.print(Text(message, style=f"bold {DORF_BRIGHT_MOSS}"))
            return
        if message in self._ACTIVITY_MESSAGES:
            self._start_activity(message)
            return
        if message == "Verifying the complete Worker loop":
            self._stop_activity()
            self._console.print(Text(message, style=f"bold {DORF_BRIGHT_MOSS}"))
            self._start_activity("Creating disposable Room")
            return
        if message.startswith("Downloading verified image"):
            self._stop_activity()
            self._console.print(Text(message, style=DORF_MAIN_TEXT))
            return
        if message.startswith("Reusing verified image"):
            self._stop_activity()
            self._print_success(message)
            return
        if message in {
            "Download digest verified",
            "Imported image fingerprint verified",
        }:
            self._stop_activity()
            self._print_success(message)
            return
        if message == "✓ Disposable Room created":
            self._stop_activity()
            self._print_success(message[2:])
            self._start_activity("Waiting for Codex to complete a real turn")
            return
        if message == "✓ Codex completed a real turn":
            self._stop_activity()
            self._print_success(message[2:])
            self._start_activity("Cleaning up disposable verification")
            return
        if message.startswith("✓ "):
            self._stop_activity()
            self._print_success(message[2:])
            return
        if message.startswith("! "):
            self._stop_activity()
            self._console.print(Text(message, style=f"bold {DORF_ACTIVE}"))
            return
        if message.startswith("  dorf "):
            self._console.print(Text(message, style=DORF_MAIN_TEXT))
            return
        self._console.print(Text(message))

    def update_image_download(self, downloaded: int, total: int) -> None:
        self._stop_activity()
        self._image_progress.update(downloaded, total)

    def finish(self) -> None:
        self._stop_activity()
        self._image_progress.finish()

    def _start_activity(self, message: str) -> None:
        self._stop_activity()
        self._status = Status(
            Text(message, style=f"bold {DORF_ACTIVE}"),
            console=self._console,
            spinner="dots",
            spinner_style=DORF_ACTIVE,
        )
        self._status.start()

    def _stop_activity(self) -> None:
        if self._status is not None:
            self._status.stop()
            self._status = None

    def _print_success(self, message: str) -> None:
        line = Text()
        line.append("✓ ", style=f"bold {DORF_BRIGHT_MOSS}")
        line.append(message)
        self._console.print(line)


def _format_download_size(size: int) -> str:
    if size >= 1024 * 1024:
        return f"{size / (1024 * 1024):.0f} MiB"
    if size >= 1024:
        return f"{size / 1024:.0f} KiB"
    return f"{size} B"


@dataclass(frozen=True)
class GitTarget:
    repo: Path
    branch: str
    start_sha: str


@dataclass(frozen=True)
class CodingTask:
    summary: str
    prompt: str


@dataclass(frozen=True)
class GitBackedJobBranch:
    repo_full_name: str
    base_sha: str
    metadata: dict[str, str]
    token: str


@app.callback(invoke_without_command=True)
def main(
    version: bool = typer.Option(False, "--version", help="Show the dorf version."),
) -> None:
    if version:
        typer.echo(__version__)
        raise typer.Exit


@app.command()
def setup() -> None:
    """Prepare Dorf and prove the complete local Worker path."""
    display = _SetupDisplay()
    display.emit("◆ Dorf")
    display.emit("  Durable workers in private local Rooms")
    display.emit("")
    display.emit("Checking this machine")
    try:
        result = CoreSetup(
            provider_connector=connect_setup_provider,
            incus_installer=install_setup_incus,
            incus_access_repairer=repair_setup_incus_access,
            incus_initializer=initialize_setup_incus,
            image_progress=display.update_image_download,
        ).run(emit=display.emit)
    except CoreSetupPaused as error:
        display.finish()
        echo_setup_stop(error)
        raise typer.Exit(1) from error
    except CoreSetupFailed as error:
        display.finish()
        echo_setup_stop(error)
        raise typer.Exit(1) from error
    finally:
        display.finish()

    display.emit("")
    display.emit("Dorf is ready.")
    display.emit(f"AI provider: {result.provider_connection}")
    display.emit(f"Image: {result.image_fingerprint[:12]}")
    display.emit("")
    display.emit("Next:")
    display.emit("  dorf worker spawn my-worker")


def echo_setup_stop(error: CoreSetupPaused | CoreSetupFailed) -> None:
    """Render one stable result and persist its portable diagnostic bundle."""
    echo_diagnostic_stop(
        "Setup",
        error.to_diagnostic(),
        resume_command="dorf setup",
    )


def echo_diagnostic_stop(
    command_label: str,
    diagnostic: SetupDiagnostic,
    *,
    resume_command: str,
) -> None:
    """Render and persist one bounded diagnostic result."""
    diagnostic = sanitize_setup_diagnostic(diagnostic)
    typer.echo()
    typer.echo(f"{command_label} {diagnostic.status}", err=True)
    typer.echo(diagnostic.summary, err=True)
    bundle: Path | None = None
    try:
        bundle = write_setup_diagnostic(diagnostic)
    except OSError as bundle_error:
        typer.echo(
            f"Diagnostic bundle unavailable: {bundle_error}",
            err=True,
        )
    if bundle is not None:
        typer.echo("Human-readable diagnostic:", err=True)
        typer.echo(f"  {bundle / 'diagnostic.md'}", err=True)
        typer.echo("Agent-readable diagnostic:", err=True)
        typer.echo(f"  {bundle / 'diagnostic.json'}", err=True)
    if diagnostic.safe_actions:
        typer.echo(f"Next: {diagnostic.safe_actions[0]}", err=True)
    typer.echo(f"Then rerun: {resume_command}", err=True)


def install_setup_incus(probe: IncusRunnerProbe) -> None:
    """Explain and apply one exact reviewed host installation recipe."""
    try:
        recipe = supported_incus_host_recipe()
        host_label = host_os_label()
    except HostSetupError as error:
        raise CoreSetupPaused(
            f"Dorf could not identify this Linux distribution: {error}",
            remediation=(
                "Install Incus using https://linuxcontainers.org/incus/docs/main/installing/."
            ),
            owner="host",
            classification="unsupported",
        ) from error
    if recipe is None:
        raise CoreSetupPaused(
            f"Automatic Incus installation is not supported for {host_label or 'this host'}.",
            remediation=(
                "Install Incus using https://linuxcontainers.org/incus/docs/main/installing/."
            ),
            owner="host",
            classification="unsupported",
        )
    typer.echo()
    typer.echo("Incus is not installed")
    typer.echo("Incus provides the isolated virtual machines Dorf calls Rooms.")
    typer.echo("Installing it will:")
    if recipe == "arch":
        typer.echo("• update Arch packages required by the rolling-release package set")
        typer.echo("• install Arch's Incus package")
    else:
        typer.echo("• refresh Ubuntu package metadata")
        typer.echo("• install Ubuntu's native Incus and QEMU VM packages")
    typer.echo("• enable and start the local Incus service")
    typer.echo("• add your user to incus-admin, which has root-equivalent machine access")
    typer.echo("No remote Incus API will be enabled.")
    if not typer.confirm("Install Incus now?", default=True):
        raise CoreSetupPaused(
            "Incus installation was declined; no machine changes were made.",
            remediation="Install Incus when you are ready.",
            owner="host",
            approval_required_actions=("Install and enable Incus on this host.",),
        )
    try:
        if recipe == "arch":
            install_incus_on_arch(probe)
        else:
            install_incus_on_ubuntu_2404(probe)
    except HostSetupError as error:
        raise CoreSetupFailed(
            f"Incus installation failed: {error}",
            owner="packaging",
        ) from error
    typer.echo("✓ Incus installed and local service enabled")


def initialize_setup_incus(
    probe: IncusRunnerProbe,
    config: IncusConfig,
) -> None:
    """Explain and apply Incus's bounded pristine local initialization."""
    typer.echo()
    typer.echo("Incus needs local storage and a private VM network")
    typer.echo("Dorf will create:")
    typer.echo("• a local directory-backed storage pool")
    typer.echo(f"• a private NAT bridge named {config.network}")
    typer.echo("No remote Incus API will be enabled.")
    if not typer.confirm("Initialize Incus now?", default=True):
        raise CoreSetupPaused(
            "Incus initialization was declined; no resources were created.",
            remediation="Initialize Incus when you are ready.",
            owner="incus",
            approval_required_actions=(
                "Create local Incus storage and the private incusbr0 network.",
            ),
        )
    try:
        initialize_pristine_incus(probe, config=config)
    except HostSetupError as error:
        raise CoreSetupFailed(
            f"Incus initialization failed: {error}",
            owner="incus",
            classification="configuration",
        ) from error
    typer.echo("✓ Local Incus storage initialized")
    typer.echo(f"✓ Private VM network created · {config.network}")


def repair_setup_incus_access(probe: IncusRunnerProbe) -> None:
    """Resume only reviewed service and administrator-group changes."""
    try:
        state = inspect_incus_host(probe)
    except HostSetupError as error:
        raise CoreSetupPaused(
            f"Dorf could not inspect this Incus installation: {error}",
            remediation=(
                "Follow https://linuxcontainers.org/incus/docs/main/installing/ for this host."
            ),
            owner="host",
        ) from error
    if state.needs_privileged_repair:
        _explain_incus_access_repair(state)
        if not typer.confirm("Repair local Incus access now?", default=True):
            raise CoreSetupPaused(
                "Incus access repair was declined; no machine changes were made.",
                remediation="Repair the reported Incus service or group state when ready.",
                owner="host",
                approval_required_actions=(
                    "Enable the Incus service or configure incus-admin membership.",
                ),
            )
        try:
            repair_incus_host(probe, state=state)
        except HostSetupError as error:
            raise CoreSetupFailed(
                f"Incus access repair failed: {error}",
                owner="host",
                classification="configuration",
            ) from error
        typer.echo("✓ Local Incus service and administrator membership repaired")
    if not state.admin_membership_effective and os.geteuid() != 0:
        raise CoreSetupPaused(
            "Incus administrator membership is configured but not active in this login.",
            remediation="Sign out and back in so incus-admin membership takes effect.",
            owner="host",
        )


def _explain_incus_access_repair(state: IncusHostState) -> None:
    typer.echo()
    typer.echo("Incus is installed, but its local access setup is incomplete.")
    typer.echo("Dorf needs administrator permission to:")
    if not state.service_enabled or not state.service_active:
        typer.echo("• enable and start the local Incus service")
    elif state.service_restart_required:
        typer.echo("• restart the local Incus service to activate its installed package update")
    if not state.admin_membership_configured:
        typer.echo("• add your user to incus-admin, which has root-equivalent access")
    typer.echo("No remote Incus API will be enabled.")


def connect_setup_provider(gateway: ProviderGateway) -> str:
    """Guide one first-run provider choice without exposing connection plumbing."""
    typer.echo("No AI model provider is connected.")
    typer.echo("1. ChatGPT subscription · recommended")
    typer.echo("2. OpenAI API key")
    choice_prompt = "Choose 1 or 2"
    if "NO_COLOR" not in os.environ:
        choice_prompt = typer.style(
            choice_prompt,
            fg=DORF_ACTIVE_RGB,
            bold=True,
        )
    choice = typer.prompt(choice_prompt, default="1")
    while choice not in {"1", "2"}:
        typer.echo(
            "Enter 1 for ChatGPT subscription or 2 for OpenAI API key.",
            err=True,
        )
        choice = typer.prompt(choice_prompt, default="1")
    if choice == "1":
        connection = gateway.connect_chatgpt_subscription(
            name="personal-chatgpt",
            on_authorization=echo_device_authorization,
        )
    else:
        credential = os.environ.get("OPENAI_API_KEY")
        if not credential:
            credential = typer.prompt("OpenAI API key", hide_input=True)
        connection = gateway.connect_openai_api_key(
            name="openai-api",
            api_key=credential,
        )
    typer.echo(f"✓ Connected · {connection.name}")
    return connection.name


@provider_app.command("connect")
def provider_connect(
    provider: str = typer.Argument(..., help="Provider to connect."),
    name: str = typer.Option(..., "--name", help="Stable connection name."),
    subscription: bool = typer.Option(
        False,
        "--subscription",
        help="Connect an interactive subscription.",
    ),
    api_key: bool = typer.Option(
        False,
        "--api-key",
        help="Connect an API key read from the provider environment or a hidden prompt.",
    ),
) -> None:
    """Connect one named upstream provider credential."""
    provider = provider.strip().lower()
    try:
        with ProviderGateway.open() as gateway:
            if provider == "chatgpt" and subscription and not api_key:
                connection = gateway.connect_chatgpt_subscription(
                    name=name,
                    on_authorization=echo_device_authorization,
                )
            elif provider in {"openai", "deepseek"} and api_key and not subscription:
                credential = os.environ.get(f"{provider.upper()}_API_KEY")
                if not credential:
                    credential = typer.prompt(f"{provider.title()} API key", hide_input=True)
                connector = (
                    gateway.connect_openai_api_key
                    if provider == "openai"
                    else gateway.connect_deepseek_api_key
                )
                connection = connector(name=name, api_key=credential)
            else:
                typer.echo(
                    "Choose chatgpt --subscription, openai --api-key, or deepseek --api-key",
                    err=True,
                )
                raise typer.Exit(2)
    except (ProviderGatewayError, ValueError) as error:
        echo_provider_error(error)
        raise typer.Exit(1) from error
    if connection.provider == "deepseek":
        echo_provider_connection(connection)
        typer.echo("Reviewer connection kept out of the deployment default.")
        return
    try:
        set_default_provider_connection(connection.name)
    except (DeploymentProfileError, OSError) as error:
        echo_provider_connection(connection)
        typer.echo(
            f"Connected, but could not select {connection.name} as the default: {error}",
            err=True,
        )
        raise typer.Exit(1) from error
    echo_provider_connection(connection)
    typer.echo(f"Default for new Rooms: {connection.name}")


@provider_app.command("list")
def provider_list() -> None:
    """List durable provider connections without exposing credentials."""
    try:
        with ProviderGateway.open() as gateway:
            connections = gateway.list_connections()
    except ProviderGatewayError as error:
        echo_provider_error(error)
        raise typer.Exit(1) from error
    if not connections:
        typer.echo("No provider connections.")
        return
    for connection in connections:
        echo_provider_connection(connection)


@provider_app.command("status")
def provider_status(
    name: str = typer.Argument(..., help="Stable connection name."),
) -> None:
    """Inspect one provider connection and its remediation."""
    try:
        with ProviderGateway.open() as gateway:
            connection = gateway.connection_status(name)
    except (ProviderGatewayError, ValueError) as error:
        echo_provider_error(error)
        raise typer.Exit(1) from error
    echo_provider_connection(connection)
    if connection.plan is not None:
        typer.echo(f"plan: {connection.plan}")
    if connection.remediation is not None:
        typer.echo(f"remediation: {connection.remediation}")
    if connection.status != "connected":
        raise typer.Exit(1)


@provider_app.command("disconnect")
def provider_disconnect(
    name: str = typer.Argument(..., help="Stable connection name."),
) -> None:
    """Disconnect one provider connection and invalidate its upstream credential."""
    try:
        with ProviderGateway.open() as gateway:
            removed = gateway.disconnect_connection(name)
    except (ProviderGatewayError, ValueError) as error:
        echo_provider_error(error)
        raise typer.Exit(1) from error
    if not removed:
        typer.echo(f"Provider connection not found: {name}", err=True)
        typer.echo("remediation: Run: dorf provider list", err=True)
        raise typer.Exit(1)
    typer.echo(f"Disconnected provider connection: {name}")


def echo_device_authorization(authorization: DeviceAuthorization) -> None:
    typer.echo(f"Open: {authorization.verification_url}")
    typer.echo(f"Code: {authorization.user_code}")


def echo_provider_connection(connection: ProviderConnection) -> None:
    typer.echo(
        f"{connection.name} · {connection.provider} · {connection.auth_mode} · {connection.status}"
    )


def echo_provider_error(error: Exception) -> None:
    typer.echo(str(error), err=True)
    remediation = getattr(error, "remediation", None)
    if isinstance(remediation, str) and remediation:
        typer.echo(f"remediation: {remediation}", err=True)


@worker_app.command("spawn")
def worker_spawn(
    name: str = typer.Argument(..., help="Stable Worker name."),
    provider_connection: str | None = typer.Option(
        None,
        "--provider-connection",
        help="Override the global Provider Connection for this Room.",
    ),
) -> None:
    """Summon a Worker into its initial Room without creating a Job."""
    try:
        profile = load_optional_deployment_profile()
    except DeploymentProfileError as error:
        typer.echo(f"Could not load the global deployment profile: {error}", err=True)
        raise typer.Exit(1) from error
    selected_provider = provider_connection or (
        profile.provider_connection if profile is not None else None
    )
    if selected_provider is None:
        typer.echo(
            "Dorf setup is incomplete: no default Provider Connection is configured.",
            err=True,
        )
        typer.echo(
            "remediation: Run: dorf provider connect --help",
            err=True,
        )
        raise typer.Exit(1)
    dorf = open_dorf(
        deployment_profile=profile,
        provider_connection=selected_provider,
    )
    try:
        binding = dorf.spawn_worker(name)
    except EnvironmentPrerequisitesError as error:
        echo_environment_prerequisite_failures(error.failures)
        raise typer.Exit(1) from error
    except (InvalidWorkerNameError, RuntimeError, ValueError) as error:
        typer.echo(f"Could not spawn Worker {name}: {error}", err=True)
        raise typer.Exit(1) from error
    echo_worker_binding(
        binding,
        current_job_name=dorf.current_job_for_worker(binding.worker.name),
    )
    if binding.room.status != "ready" or binding.worker.status not in {
        "ready",
        "assigned",
    }:
        raise typer.Exit(1)


def echo_worker_binding(
    binding: WorkerBinding,
    *,
    current_job_name: str | None = None,
) -> None:
    worker = binding.worker
    room = binding.room
    typer.echo(f"{worker.name} · {worker.status}")
    typer.echo(f"harness: {worker.harness_type}")
    typer.echo(f"provenance: {worker.provenance}")
    typer.echo(f"lifecycle policy: {worker.lifecycle_policy}")
    typer.echo(f"room: {room.status} ({room.provider_id})")
    typer.echo(f"workspace: {room.workspace}")
    typer.echo(f"general conversation: {worker.general_conversation_id or 'not started'}")
    typer.echo(f"current Job: {current_job_name or 'none'}")
    if worker.error:
        typer.echo(f"Worker error: {worker.error}", err=True)
    if room.error:
        typer.echo(f"Room error: {room.error}", err=True)


@worker_app.command("end")
def worker_end(
    name: str = typer.Argument(..., help="Worker identity to end."),
    interrupt: bool = typer.Option(False, "--interrupt", help="Cancel unsettled direct work."),
) -> None:
    """Destroy an idle Worker's exact Room and retain its ended identity."""
    dorf = open_dorf()
    try:
        result = dorf.end_worker(name, interrupt=interrupt)
    except DorfResourceNotFoundError as error:
        typer.echo(str(error), err=True)
        raise typer.Exit(1) from error
    except (WorkerUnsettledError, RuntimeError) as error:
        typer.echo(f"Could not end Worker {name}: {error}", err=True)
        raise typer.Exit(1) from error
    if result.already_ended:
        typer.echo(f"Worker already ended: {name}")
        return
    typer.echo(f"Ended Worker: {result.worker.name}")
    assert result.room is not None
    typer.echo(f"Room destroyed: {result.room.provider_id}")


@worker_app.command("recover")
def worker_recover(
    name: str = typer.Argument(..., help="Worker identity to recover."),
) -> None:
    """Reconcile current-Room conversations and restart replaceable controllers."""
    dorf = open_dorf()
    try:
        result = dorf.recover_worker(name)
    except InvalidWorkerNameError as error:
        typer.echo(f"Invalid Worker name: {error}", err=True)
        raise typer.Exit(1) from error
    except DorfResourceNotFoundError as error:
        typer.echo(str(error), err=True)
        raise typer.Exit(1) from error
    except RuntimeError as error:
        typer.echo(f"Could not recover Worker {name}: {error}", err=True)
        raise typer.Exit(1) from error
    typer.echo(f"Recovered Worker: {name}")
    typer.echo(f"Room: preserved ({result.binding.room.provider_id}, {result.room_outcome})")
    typer.echo(f"Worker turns reconciled: {len(result.worker_turns)}")
    typer.echo(f"Job turns reconciled: {len(result.job_turns)}")
    typer.echo(
        "Worker delivery: "
        + ("started" if result.worker_dispatcher_started else "already running or unavailable")
    )
    if result.job_name:
        typer.echo(
            "Job delivery: "
            + ("started" if result.job_dispatcher_started else "already running or unavailable")
        )
        collector_status = (
            "started" if result.report_collector_started else "already running or unavailable"
        )
        typer.echo(f"Report collector: {collector_status}")


@worker_app.command("attach")
def worker_attach(
    name: str = typer.Argument(..., help="Worker whose current Room to enter."),
) -> None:
    """Enter the current Room at /workspace until the interactive shell exits."""
    dorf = open_dorf()
    try:
        binding = dorf.get_worker_binding(name)
    except InvalidWorkerNameError as error:
        typer.echo(f"Invalid Worker name: {error}", err=True)
        raise typer.Exit(1) from error
    if binding is None:
        worker = dorf.get_worker(name)
        detail = "not found" if worker is None else "offline with no current Room"
        typer.echo(f"Could not attach to Worker {name}: {detail}", err=True)
        raise typer.Exit(1)
    typer.echo(f"Entering {name} Room at {binding.room.workspace}. Exit the shell to leave.")
    try:
        with attachment_interrupt_handlers():
            result = dorf.attach_worker(name)
    except (WorkerAlreadyAttachedError, WorkerOfflineError, RuntimeError) as error:
        typer.echo(f"Could not attach to Worker {name}: {error}", err=True)
        raise typer.Exit(1) from error
    except KeyboardInterrupt as error:
        typer.echo(f"Attachment interrupted: {name}", err=True)
        raise typer.Exit(130) from error
    typer.echo(f"Attachment ended: {result.worker_name}")
    if result.exit_code != 0:
        raise typer.Exit(result.exit_code)


@contextmanager
def attachment_interrupt_handlers() -> Iterator[None]:
    """Turn ordinary terminal disconnect signals into unwindable cleanup."""
    previous = {signum: signal.getsignal(signum) for signum in (signal.SIGHUP, signal.SIGTERM)}

    def interrupt(signum, frame) -> None:
        raise KeyboardInterrupt

    try:
        for signum in previous:
            signal.signal(signum, interrupt)
        yield
    finally:
        for signum, handler in previous.items():
            signal.signal(signum, handler)


@worker_app.command("inspect")
def worker_inspect(
    name: str = typer.Argument(..., help="Worker name."),
) -> None:
    """Inspect Worker, general-conversation, and current-Room facts read-only."""
    dorf = open_dorf()
    try:
        inspection = dorf.inspect_worker(name)
    except InvalidWorkerNameError as error:
        typer.echo(f"Invalid Worker name: {error}", err=True)
        raise typer.Exit(1) from error
    except DorfResourceNotFoundError as error:
        typer.echo(str(error), err=True)
        raise typer.Exit(1) from error
    except RuntimeError as error:
        typer.echo(f"Could not inspect Worker {name}: {error}", err=True)
        raise typer.Exit(1) from error
    echo_worker_inspection(inspection)


def echo_worker_inspection(inspection: WorkerInspection) -> None:
    worker = inspection.worker
    room = inspection.room
    effective_status = (
        "ended"
        if worker.status == "ended"
        else worker.status
        if inspection.room_observation == "available"
        else "offline"
    )
    typer.echo(f"{worker.name} · {effective_status}")
    typer.echo(f"harness: {worker.harness_type}")
    typer.echo(f"provenance: {worker.provenance}")
    typer.echo(f"lifecycle policy: {worker.lifecycle_policy}")
    if room is None:
        typer.echo("room: absent")
        typer.echo("workspace: unavailable")
    else:
        room_line = f"room: {inspection.room_observation} (recorded {room.status})"
        if inspection.room_observation_error:
            room_line = f"{room_line}: {inspection.room_observation_error}"
        typer.echo(room_line)
        typer.echo(f"workspace: {room.workspace}")
    if inspection.presence is None:
        typer.echo("human presence: detached")
    else:
        typer.echo(
            f"human presence: attached at {inspection.presence.workspace} "
            f"since {inspection.presence.attached_at}"
        )
    conversation = inspection.conversation
    if conversation is None:
        typer.echo("general: not started")
    else:
        native = conversation.native_conversation_id or "native thread not started"
        typer.echo(f"general: {conversation.status} ({native})")
        typer.echo(f"conversation defaults: {conversation.model} ({conversation.reasoning_effort})")
    if inspection.latest_turn is None:
        typer.echo("activity: no turn delivered")
    else:
        turn = inspection.latest_turn
        typer.echo(f"activity: direct message {turn.status} ({turn.phase})")
    typer.echo(f"queued messages: {inspection.queued_messages}")
    typer.echo(f"current Job: {inspection.current_job_name or 'none'}")
    typer.echo(f"facts observed: {inspection.observed_at}")
    typer.echo(f"worker updated: {worker.updated_at}")


@worker_app.command("wait")
def worker_wait(
    name: str = typer.Argument(..., help="Worker name."),
    message_id: str | None = typer.Option(
        None, "--message", help="Wait for this exact admitted message ID."
    ),
    timeout: float | None = typer.Option(
        None,
        "--timeout",
        min=0,
        help="Stop waiting after this many seconds.",
    ),
    json_output: bool = typer.Option(False, "--json", help="Emit a structured outcome."),
) -> None:
    """Wait read-only for one pinned direct-message outcome."""
    dorf = open_dorf()
    try:
        result = dorf.wait_for_worker_message(
            name,
            message_id=message_id,
            timeout=timeout,
        )
    except InvalidWorkerNameError as error:
        typer.echo(f"Invalid Worker name: {error}", err=True)
        raise typer.Exit(1) from error
    except DorfResourceNotFoundError as error:
        typer.echo(str(error), err=True)
        raise typer.Exit(1) from error
    except RuntimeError as error:
        typer.echo(f"Could not wait for Worker {name}: {error}", err=True)
        raise typer.Exit(1) from error
    echo_worker_wait_result(name, result, json_output=json_output)
    if result.outcome == "working" and timeout is not None:
        raise typer.Exit(75)


def echo_worker_wait_result(
    name: str,
    result: WorkerWaitResult,
    *,
    json_output: bool,
) -> None:
    payload = {
        "detail": result.detail,
        "message_id": result.message_id,
        "observed_at": result.observed_at,
        "outcome": result.outcome,
        "response": result.response,
        "sequence": result.sequence,
        "worker": name,
    }
    if json_output:
        typer.echo(json.dumps(payload, sort_keys=True))
        return
    typer.echo(f"Worker {name}: {result.outcome}")
    typer.echo(f"Message: {result.sequence} ({result.message_id})")
    if result.response:
        typer.echo("Response:")
        typer.echo(result.response)
    if result.detail:
        label = "Need" if result.outcome in {"blocked", "pending-approval"} else "Detail"
        typer.echo(f"{label}: {result.detail}")
    typer.echo(f"Observed: {result.observed_at}")


@worker_app.command("message")
def worker_message(
    name: str = typer.Argument(..., help="Worker name."),
    message: str = typer.Argument(..., help="Natural-language direct message."),
    model: str | None = typer.Option(
        None,
        "--model",
        help="Codex model for this turn; the first message also sets the default.",
    ),
    reasoning_effort: str | None = typer.Option(
        None,
        "--reasoning-effort",
        help="Reasoning effort for this turn; the first message also sets the default.",
    ),
    json_output: bool = typer.Option(False, "--json", help="Emit a structured receipt."),
) -> None:
    """Admit one durable direct message and start detached delivery."""
    dorf = open_dorf()
    try:
        worker = dorf.get_worker(name)
    except InvalidWorkerNameError as error:
        typer.echo(f"Invalid Worker name: {error}", err=True)
        raise typer.Exit(1) from error
    if worker is None:
        typer.echo(f"Could not message Worker {name}: Worker not found", err=True)
        raise typer.Exit(1)
    try:
        admitted = dorf.message_worker(
            name,
            message,
            model=model,
            reasoning_effort=reasoning_effort,
        )
    except (
        DorfResourceNotFoundError,
        AgentConfigValidationError,
        InvalidWorkerNameError,
        RuntimeError,
        ValueError,
    ) as error:
        typer.echo(f"Could not message Worker {name}: {error}", err=True)
        raise typer.Exit(1) from error

    receipt = {
        "delivery": "started" if admitted.dispatcher_started else "pending",
        "message_id": admitted.message.id,
        "sequence": admitted.message.sequence,
        "status": "queued",
        "worker": name,
    }
    if json_output:
        typer.echo(json.dumps(receipt, sort_keys=True))
        return
    typer.echo(f"Queued direct message {receipt['sequence']} for Worker {name}")
    if receipt["delivery"] == "started":
        typer.echo("Delivery dispatcher started")
    else:
        typer.echo("Delivery pending; the durable queue item was retained")


@job_app.command("assign")
def job_assign(
    name: str = typer.Argument(..., help="Stable Job name."),
    worker_name: str = typer.Option(..., "--to", help="Existing ready Worker."),
    goal: str = typer.Option(..., "--goal", help="Complete goal version 1."),
    model: str | None = typer.Option(None, "--model", help="Job conversation model."),
    reasoning_effort: str | None = typer.Option(
        None,
        "--reasoning-effort",
        help="Job conversation reasoning effort.",
    ),
) -> None:
    """Create a goal-backed Job and assign it to an existing Worker."""
    dorf = open_dorf()
    try:
        worker = dorf.get_worker(worker_name)
    except InvalidWorkerNameError as error:
        typer.echo(f"Invalid Worker name: {error}", err=True)
        raise typer.Exit(1) from error
    if worker is None:
        typer.echo(f"Could not assign Job {name}: Worker not found: {worker_name}", err=True)
        raise typer.Exit(1)
    worker_binding = dorf.get_worker_binding(worker_name)
    if worker_binding is None:
        typer.echo(f"Could not assign Job {name}: Worker is Roomless: {worker_name}", err=True)
        raise typer.Exit(1)
    try:
        result = dorf.assign_job(
            name,
            worker_name=worker_name,
            goal=goal,
            model=model,
            reasoning_effort=reasoning_effort,
        )
    except (
        DorfResourceNotFoundError,
        AgentConfigValidationError,
        InvalidJobNameError,
        InvalidWorkerNameError,
        RuntimeError,
        ValueError,
    ) as error:
        typer.echo(f"Could not assign Job {name}: {error}", err=True)
        raise typer.Exit(1) from error

    binding = result.binding
    typer.echo(f"{binding.job.name} · {binding.job.status}")
    typer.echo(f"goal v1: {binding.job.goal}")
    typer.echo(
        f"assigned: {binding.assignment.worker_name} (generation {binding.assignment.generation})"
    )
    typer.echo(f"workspace: {binding.assignment.workspace}")
    typer.echo(
        f"Initial goal {result.initial_input.sequence} queued "
        f"({result.initial_input.id}); delivery dispatcher started"
        if result.dispatcher_started
        else (
            f"Initial goal {result.initial_input.sequence} queued "
            f"({result.initial_input.id}); dispatcher unavailable"
        )
    )


@job_app.command("message")
def job_message(
    name: str = typer.Argument(..., help="Job name."),
    message: str = typer.Argument(..., help="Natural-language Job message."),
    model: str | None = typer.Option(None, "--model", help="Model override for this turn."),
    reasoning_effort: str | None = typer.Option(
        None, "--reasoning-effort", help="Reasoning-effort override for this turn."
    ),
    json_output: bool = typer.Option(False, "--json", help="Emit a structured receipt."),
) -> None:
    """Admit one ordinary Job input without changing its pinned goal."""
    dorf = open_dorf()
    try:
        admitted = dorf.message_job(
            name,
            message,
            model=model,
            reasoning_effort=reasoning_effort,
        )
    except InvalidJobNameError as error:
        typer.echo(f"Invalid Job name: {error}", err=True)
        raise typer.Exit(1) from error
    except (
        DorfResourceNotFoundError,
        AgentConfigValidationError,
        RuntimeError,
        ValueError,
    ) as error:
        typer.echo(f"Could not message Job {name}: {error}", err=True)
        raise typer.Exit(1) from error
    receipt = {
        "delivery": "started" if admitted.dispatcher_started else "pending",
        "job": name,
        "message_id": admitted.job_input.id,
        "sequence": admitted.job_input.sequence,
        "status": "queued",
    }
    if json_output:
        typer.echo(json.dumps(receipt, sort_keys=True))
        return
    typer.echo(f"Queued message {admitted.job_input.sequence} for Job {name}")
    typer.echo(
        "Delivery dispatcher started"
        if admitted.dispatcher_started
        else "Delivery pending; the durable queue item was retained"
    )


@job_app.command("end")
def job_end(
    name: str = typer.Argument(..., help="Job identity to end."),
    interrupt: bool = typer.Option(False, "--interrupt", help="Cancel unsettled Job work."),
) -> None:
    """Cooperatively end a Job, remove its workspace, and retain its records."""
    dorf = open_dorf()
    try:
        result = dorf.end_job(name, interrupt=interrupt)
    except DorfResourceNotFoundError as error:
        typer.echo(str(error), err=True)
        raise typer.Exit(1) from error
    except JobUnsettledError as error:
        typer.echo(f"Could not end Job {name}: {error}", err=True)
        unsettled = dorf.observe_unsettled_job_input(name)
        if unsettled is not None:
            echo_job_resource_wait_result(
                name,
                unsettled,
                json_output=False,
            )
        raise typer.Exit(1) from error
    except DedicatedWorkerCleanupError as error:
        typer.echo(
            f"Job ended, but dedicated Worker cleanup remains retryable: {error}",
            err=True,
        )
        raise typer.Exit(1) from error
    except RuntimeError as error:
        typer.echo(f"Could not end Job {name}: {error}", err=True)
        raise typer.Exit(1) from error
    ended = result.binding
    typer.echo(f"Ended Job: {ended.job.name}")
    typer.echo(f"Released Worker: {ended.worker.name}")
    if ended.room.status == "absent" and ended.worker.current_room_id is None:
        typer.echo("Room already absent; local workspace and processes were already gone")
    else:
        typer.echo(f"Removed workspace: {ended.assignment.workspace}")
    if result.dedicated_worker is not None:
        typer.echo(f"Ended dedicated Worker: {result.dedicated_worker.name}")


@job_app.command("inspect")
def job_inspect(
    name: str = typer.Argument(..., help="Job name."),
    timeline: bool = typer.Option(False, "--timeline", help="Show the chronological Job story."),
    evidence: bool = typer.Option(
        False, "--evidence", help="List accepted artifacts with provenance."
    ),
    json_output: bool = typer.Option(False, "--json", help="Emit the structured Job pulse."),
) -> None:
    """Inspect a Job pulse or one read-only document lens."""
    if sum((timeline, evidence, json_output)) > 1:
        typer.echo("Choose only one of --timeline, --evidence, or --json", err=True)
        raise typer.Exit(2)
    dorf = open_dorf()
    try:
        events = dorf.job_timeline(name)
    except InvalidJobNameError as error:
        typer.echo(f"Invalid Job name: {error}", err=True)
        raise typer.Exit(1) from error
    except DorfResourceNotFoundError as error:
        typer.echo(str(error), err=True)
        raise typer.Exit(1) from error
    if timeline:
        echo_job_timeline(name, events)
        return
    if evidence:
        echo_job_evidence(name, events, dorf.job_documents_path(name))
        return
    try:
        inspection = dorf.inspect_job(name)
    except RuntimeError as error:
        typer.echo(f"Could not inspect Job {name}: {error}", err=True)
        raise typer.Exit(1) from error
    coding_store = CodingStore.open()
    if coding_store.get_coding_job(name) is not None:
        pulse = build_coding_job_pulse(coding_store, inspection)
        echo_coding_job_pulse(pulse, json_output=json_output)
        return
    if json_output:
        typer.echo("Structured outcome pulses are currently available for coding Jobs.", err=True)
        raise typer.Exit(1)
    echo_job_inspection(inspection, events)


def echo_coding_job_pulse(pulse: CodingJobPulse, *, json_output: bool) -> None:
    if json_output:
        typer.echo(json.dumps(asdict(pulse), sort_keys=True))
        return
    typer.echo(f"{pulse.job} · {pulse.outcome_stage}")
    typer.echo(f"goal v{pulse.goal_version}: {pulse.goal_summary}")
    lifecycle = pulse.lifecycle
    typer.echo(
        f"lifecycle [{lifecycle.source} {lifecycle.provenance}]: {lifecycle.state}"
    )
    room = pulse.room_availability
    room_detail = f" ({room.detail})" if room.detail else ""
    typer.echo(
        f"Room availability [{room.source} {room.provenance}]: {room.status}{room_detail}"
    )
    delta = pulse.latest_delta
    typer.echo(f"delta [{delta.source} {delta.provenance}]: {delta.summary}")
    activity = pulse.observed_activity
    typer.echo(f"activity [{activity.status}]: {activity.detail}")
    typer.echo(f"claim support: {activity.claim_support}")
    if pulse.worker_claim is None:
        typer.echo("latest Worker claim: none accepted")
    else:
        typer.echo(f"latest Worker claim [claim]: {pulse.worker_claim.summary}")
    typer.echo(f"evidence: {pulse.evidence_count} accepted")
    typer.echo(f"attention: {pulse.attention.state} ({pulse.attention.reason})")
    if pulse.attention.id is not None:
        typer.echo(f"  item: {pulse.attention.id}")
        typer.echo(f"  consumer: {pulse.attention.failed_consumer}")
        typer.echo(f"  evidence: {pulse.attention.observed_evidence}")
        typer.echo(f"  owner: {pulse.attention.owner}")
        typer.echo(f"  action: {pulse.attention.exact_action}")
        typer.echo(f"  consequence: {pulse.attention.consequence}")
        typer.echo(f"  recommended default: {pulse.attention.recommended_default}")
        typer.echo(f"  expiry/decline: {pulse.attention.expiry_decline_behavior}")
        typer.echo(f"  automatic resume: {pulse.attention.automatic_resume}")
        typer.echo(f"  expires: {pulse.attention.expires_at}")
    typer.echo(f"updated: {pulse.updated_at}")


def echo_job_inspection(
    inspection: JobInspection,
    events: list[TimelineEvent] | None = None,
) -> None:
    typer.echo(f"{inspection.job.name} · {inspection.job.status}")
    typer.echo(f"goal v{inspection.job.goal_version}: {inspection.job.goal}")
    typer.echo(
        f"assigned: {inspection.assignment.worker_name} "
        f"(generation {inspection.assignment.generation})"
    )
    typer.echo(
        f"room: {inspection.room_observation} (recorded {inspection.room.status})"
        + (f": {inspection.room_observation_error}" if inspection.room_observation_error else "")
    )
    typer.echo(f"assignment status: {inspection.assignment.status}")
    typer.echo(f"workspace: {inspection.assignment.workspace}")
    native = inspection.conversation.native_conversation_id or "native thread not started"
    typer.echo(f"conversation: {inspection.conversation.status} ({native})")
    if inspection.latest_turn is None:
        typer.echo("activity: no input delivered")
    else:
        typer.echo(
            f"activity: input {inspection.latest_turn.status} ({inspection.latest_turn.phase})"
        )
    typer.echo(f"queued inputs: {inspection.queued_inputs}")
    recorded = events or []
    worker_claims = [
        event for event in recorded if event.source == "worker" and event.provenance == "claim"
    ]
    assumptions = [event for event in worker_claims if event.kind == "assumption"]
    evidence_count = sum(len(event.artifacts) for event in recorded)
    if worker_claims:
        typer.echo(f"latest Worker claim: {worker_claims[-1].summary}")
    else:
        typer.echo("latest Worker claim: none accepted")
    typer.echo(f"assumptions: {len(assumptions)} accepted")
    typer.echo(f"evidence: {evidence_count} accepted")
    if recorded:
        typer.echo(f"documents updated: {recorded[-1].recorded_at}")
    typer.echo(f"facts observed: {inspection.observed_at}")


@job_artifact_app.command("list")
def job_artifact_list(
    name: str = typer.Argument(..., help="Job name."),
) -> None:
    """List the path-free retained-artifact manifest for one Job."""
    dorf = open_dorf()
    try:
        artifacts = dorf.list_job_artifacts(name)
    except InvalidJobNameError as error:
        typer.echo(f"Invalid Job name: {error}", err=True)
        raise typer.Exit(1) from error
    except (DorfResourceNotFoundError, RuntimeError) as error:
        typer.echo(str(error), err=True)
        raise typer.Exit(1) from error
    typer.echo(f"Artifacts · {name}")
    if not artifacts:
        typer.echo("No artifacts retained.")
        return
    for artifact in artifacts:
        echo_job_artifact(artifact)


@job_artifact_app.command("export")
def job_artifact_export(
    name: str = typer.Argument(..., help="Job name."),
    artifact_ref: str = typer.Argument(
        ...,
        help="Stable artifact reference from the Job manifest.",
    ),
    destination: Path = ARTIFACT_DESTINATION_OPTION,
    overwrite: bool = ARTIFACT_OVERWRITE_OPTION,
) -> None:
    """Export one exact retained artifact without exposing its storage path."""
    dorf = open_dorf()
    try:
        result = dorf.export_job_artifact(
            name,
            artifact_ref,
            destination,
            overwrite=overwrite,
        )
    except InvalidJobNameError as error:
        typer.echo(f"Invalid Job name: {error}", err=True)
        raise typer.Exit(1) from error
    except (DorfResourceNotFoundError, OSError, RuntimeError, ValueError) as error:
        typer.echo(f"Could not export artifact: {error}", err=True)
        raise typer.Exit(1) from error
    if result.status == "missing":
        typer.echo(f"Artifact not found for Job {name}: {artifact_ref}", err=True)
        raise typer.Exit(1)
    if result.status == "cross-job":
        typer.echo("Artifact reference belongs to another Job.", err=True)
        raise typer.Exit(1)
    if result.status == "corrupt":
        typer.echo("Retained artifact failed custody verification.", err=True)
        raise typer.Exit(1)
    if result.status == "destination-exists":
        typer.echo(
            f"Destination already exists: {result.destination}. Use --overwrite to replace it.",
            err=True,
        )
        raise typer.Exit(1)
    if result.artifact is None or result.destination is None:
        raise RuntimeError("Successful artifact export omitted its result")
    typer.echo(f"Exported {result.artifact.name} to {result.destination}")
    typer.echo(
        f"{result.artifact.media_type} · {result.artifact.size} bytes · {result.artifact.digest}"
    )


@job_app.command("wait")
def job_wait_resource(
    name: str = typer.Argument(..., help="Job name."),
    message_id: str | None = typer.Option(
        None, "--message", help="Wait for this exact admitted input ID."
    ),
    timeout: float | None = typer.Option(None, "--timeout", min=0),
    json_output: bool = typer.Option(False, "--json"),
) -> None:
    """Wait read-only for one pinned Job input outcome."""
    dorf = open_dorf()
    try:
        result = dorf.wait_for_job_input(
            name,
            input_id=message_id,
            timeout=timeout,
        )
    except InvalidJobNameError as error:
        typer.echo(f"Invalid Job name: {error}", err=True)
        raise typer.Exit(1) from error
    except DorfResourceNotFoundError as error:
        typer.echo(str(error), err=True)
        raise typer.Exit(1) from error
    except RuntimeError as error:
        typer.echo(f"Could not wait for Job {name}: {error}", err=True)
        raise typer.Exit(1) from error
    echo_job_resource_wait_result(name, result, json_output=json_output)
    if result.outcome == "working" and timeout is not None:
        raise typer.Exit(75)


def echo_job_resource_wait_result(
    name: str,
    result: AssignedJobWaitResult,
    *,
    json_output: bool,
) -> None:
    payload = {
        "detail": result.detail,
        "input_id": result.input_id,
        "job": name,
        "observed_at": result.observed_at,
        "outcome": result.outcome,
        "response": result.response,
        "sequence": result.sequence,
    }
    if json_output:
        typer.echo(json.dumps(payload, sort_keys=True))
        return
    typer.echo(f"Job {name}: {result.outcome}")
    typer.echo(f"Input: {result.sequence} ({result.input_id})")
    if result.response:
        typer.echo("Response:")
        typer.echo(result.response)
    if result.detail:
        label = "Need" if result.outcome in {"blocked", "pending-approval"} else "Detail"
        typer.echo(f"{label}: {result.detail}")
    typer.echo(f"Observed: {result.observed_at}")


@app.command()
def start(
    task: str | None = typer.Argument(None, help="Coding task for the new Job."),
    issue: int | None = typer.Option(None, "--issue", min=1, help="GitHub issue to start."),
    resume: str | None = typer.Option(
        None,
        "--resume",
        help="Retry setup for this exact setup-failed coding Job.",
    ),
    provider_connection: str | None = typer.Option(
        None,
        "--provider-connection",
        help="Override the setup-selected Provider Connection.",
    ),
) -> None:
    """Create a dedicated Worker and goal-backed coding Job."""
    target = detect_git_target(Path.cwd())
    if task is not None and issue is not None:
        typer.echo("Provide exactly one of TASK or --issue.", err=True)
        raise typer.Exit(1)
    admission = None
    if issue is not None and resume is None:
        admission = prove_coding_admission_or_exit(
            target,
            command="start",
            issue_number=issue,
            model=None,
            reasoning_effort=None,
            provider_connection=provider_connection,
        )
        assert admission.issue is not None
        coding_task = github_issue_task(admission.repository, admission.issue)
    else:
        coding_task = resolve_coding_task_or_exit(
            target.repo,
            task=task,
            issue_number=issue,
        )
    launch_coding_job_or_exit(
        target,
        coding_task,
        resume_job_name=resume,
        provider_connection=provider_connection,
        admission_proof=admission,
    )


@app.command()
def afk(
    issue_number: int = typer.Argument(..., min=1, help="GitHub issue to implement."),
    model: str | None = typer.Option(None, "--model", help="Job conversation model."),
    reasoning_effort: str | None = typer.Option(
        None, "--reasoning-effort", help="Job conversation reasoning effort."
    ),
    provider_connection: str | None = typer.Option(
        None,
        "--provider-connection",
        help="Override the setup-selected Provider Connection.",
    ),
) -> None:
    """Compose unattended coding policy over the same Worker and Job runtime."""
    target = detect_git_target(Path.cwd())
    store = CodingStore.open()
    target_repo = str(target.repo.resolve())
    try:
        existing = CodingWorkflow.existing_afk_job(
            store,
            target_repo=target_repo,
            issue_number=issue_number,
        )
    except WorkflowFailure as error:
        echo_workflow_outcome(WorkflowOutcome(error.messages, error.exit_code))
        raise typer.Exit(error.exit_code) from error
    admission = None
    coding_task = None
    if existing is None:
        admission = prove_coding_admission_or_exit(
            target,
            command="afk",
            issue_number=issue_number,
            model=model,
            reasoning_effort=reasoning_effort,
            provider_connection=provider_connection,
        )
        assert admission.issue is not None
        coding_task = github_issue_task(admission.repository, admission.issue)
    else:
        proof_record = existing.metadata.get("admission_proof")
        if proof_record:
            try:
                proof_id = json.loads(proof_record).get("proof_id")
            except (json.JSONDecodeError, AttributeError):
                proof_id = None
            if isinstance(proof_id, str):
                typer.echo(f"Reusing coding admission proof: {proof_id}")
    owner_token = secrets.token_hex(16)
    try:
        afk_start = CodingWorkflow.prepare_afk_start(
            store,
            target_repo=target_repo,
            issue_number=issue_number,
            owner_token=owner_token,
        )
    except WorkflowFailure as error:
        echo_workflow_outcome(WorkflowOutcome(error.messages, error.exit_code))
        store.release_unlinked_afk_coordinator(target_repo, issue_number, owner_token)
        raise typer.Exit(error.exit_code) from error
    echo_workflow_outcome(WorkflowOutcome(afk_start.messages))
    try:
        if afk_start.action == "launch":
            assert admission is not None and coding_task is not None
            job_name = launch_coding_job_or_exit(
                target,
                coding_task,
                model=model,
                reasoning_effort=reasoning_effort,
                provider_connection=provider_connection,
                metadata={
                    "afk_issue_number": str(issue_number),
                    "afk_stage": "implementation",
                },
                admission_proof=admission,
            )
            store.link_afk_job(target_repo, issue_number, owner_token, job_name)
        else:
            assert afk_start.job_name is not None
            job_name = afk_start.job_name
    except BaseException:
        store.release_unlinked_afk_coordinator(target_repo, issue_number, owner_token)
        raise
    run_coding_job_workflow_or_exit(
        job_name,
        lambda workflow: workflow.coordinate_afk(
            issue_number=issue_number,
            target_repo=target_repo,
            owner_token=owner_token,
        ),
    )


@app.command("afk-resume")
def afk_resume(
    job_name: str,
    takeover: bool = typer.Option(
        False, "--takeover", help="Explicitly replace an interrupted coordinator owner."
    ),
    repair_attention: str | None = typer.Option(
        None,
        "--repair-attention",
        help="Approve one exact repaired authority failure for bounded automatic retry.",
    ),
    decline_attention: str | None = typer.Option(
        None,
        "--decline-attention",
        help="Decline one exact authority repair and leave the workflow visibly blocked.",
    ),
) -> None:
    """Resume an interrupted AFK coordinator for one coding Job."""
    owner_token = secrets.token_hex(16)
    store = CodingStore.open()
    try:
        resume = CodingWorkflow.prepare_afk_resume(
            store,
            job_name=job_name,
            owner_token=owner_token,
            takeover=takeover,
            repair_attention_id=repair_attention,
            decline_attention_id=decline_attention,
        )
    except WorkflowFailure as error:
        echo_workflow_outcome(WorkflowOutcome(error.messages, error.exit_code))
        raise typer.Exit(error.exit_code) from error
    echo_workflow_outcome(WorkflowOutcome(resume.messages))
    if resume.job.status in {"setting-up", "setup-failed"}:
        recover_setup_failed_coding_job_or_exit(
            store,
            resume.job,
            resolve_coding_task_or_exit(
                Path(resume.job.target_repo),
                task=None,
                issue_number=resume.issue_number,
            ),
        )
    run_coding_job_workflow_or_exit(
        job_name,
        lambda workflow: workflow.coordinate_afk(
            issue_number=resume.issue_number,
            target_repo=resume.target_repo,
            owner_token=owner_token,
        ),
    )


def launch_coding_job_or_exit(
    target: GitTarget,
    coding_task: CodingTask,
    *,
    metadata: dict[str, str] | None = None,
    model: str | None = None,
    reasoning_effort: str | None = None,
    resume_job_name: str | None = None,
    provider_connection: str | None,
    admission_proof: CodingAdmissionProof | None = None,
) -> str:
    """Compose a dedicated Worker, Job Assignment, clone, and first delivery."""
    if admission_proof is None:
        if is_dirty(target.repo):
            typer.echo("Target repo has uncommitted changes.", err=True)
            raise typer.Exit(1)
        git_author = resolve_git_author_or_exit(target.repo)
        contract = load_contract_or_exit(target.repo)
        try:
            config = resolve_codex_config(
                contract.primary_codex, model=model, reasoning_effort=reasoning_effort
            )
        except AgentConfigValidationError as error:
            typer.echo(f"Invalid Job Codex configuration: {error}", err=True)
            raise typer.Exit(1) from error
    else:
        if (
            admission_proof.target_branch != target.branch
            or admission_proof.issue is None
        ):
            typer.echo("Coding admission proof does not match this delegation.", err=True)
            raise typer.Exit(1)
        git_author = admission_proof.git_author
        contract = admission_proof.contract
        config = admission_proof.codex_config
    job_name = resume_job_name or generate_job_name(coding_task.summary)
    worker_name = f"coder-{job_name}"
    job_branch = f"dorf/{job_name}"
    validate_dorf_branch_or_exit(job_branch, target_branch=target.branch)
    store = CodingStore.open()
    existing = store.get_coding_job(job_name)
    if resume_job_name is not None and existing is None:
        typer.echo(f"Setup-failed coding Job not found: {resume_job_name}", err=True)
        raise typer.Exit(1)
    if existing is not None:
        existing_binding = store.get_job_binding(job_name)
        if existing.status not in {"setting-up", "setup-failed"}:
            typer.echo(
                f"Coding Job already exists: {job_name} ({existing.status})",
                err=True,
            )
            raise typer.Exit(1)
        if (
            Path(existing.target_repo).resolve() != target.repo.resolve()
            or existing.target_branch != target.branch
            or (existing_binding is not None and existing_binding.worker.name != worker_name)
        ):
            typer.echo(
                f"Setup-failed coding Job does not match this repository: {job_name}",
                err=True,
            )
            raise typer.Exit(1)
        recover_setup_failed_coding_job_or_exit(store, existing, coding_task)
        return job_name

    if admission_proof is None:
        selected_provider, deployment_profile = select_coding_deployment_or_exit(
            provider_connection
        )
        image_fingerprint = deployment_image_fingerprint(deployment_profile, contract)
        environment_config = IncusConfig.from_mapping(contract.incus_config)
        if image_fingerprint is not None:
            environment_config = replace(environment_config, template=image_fingerprint)
        dorf = Dorf(
            store,
            environment_config=environment_config,
            agent_defaults=contract.primary_codex,
            provider_connection=selected_provider,
            git_credential_token=github_installation_token_for_job,
        )
    else:
        selected_provider = admission_proof.provider_connection
        image_fingerprint = admission_proof.image_fingerprint
        dorf = Dorf(
            store,
            environment_config=admission_proof.environment_config,
            agent_defaults=contract.primary_codex,
            provider_connection=selected_provider,
            git_credential_token=github_installation_token_for_job,
        )
    exit_if_environment_prerequisites_missing(dorf)
    room_image_metadata = (
        {"image_fingerprint": image_fingerprint}
        if image_fingerprint is not None
        else {}
    )
    goal = coding_job_goal(
        job_name=job_name,
        task=coding_task.prompt,
        job_branch=job_branch,
        workspace=f"/workspace/jobs/{job_name}",
    )

    reservation_committed = False

    def reserve_coding_job(remote: GitBackedJobBranch) -> None:
        nonlocal reservation_committed
        coding_metadata = {
            **remote.metadata,
            **(metadata or {}),
            "task": coding_task.summary,
            "target_repo": str(target.repo),
            "target_branch": target.branch,
            "target_start_sha": remote.base_sha,
            "job_branch": job_branch,
            "setup_model": config.model,
            "setup_reasoning_effort": config.reasoning_effort,
            "setup_task_prompt": coding_task.prompt,
            "setup_provider_connection": selected_provider,
            **(
                {"setup_image_fingerprint": image_fingerprint}
                if image_fingerprint is not None
                else {}
            ),
            **(
                {
                    "admission_proof": json.dumps(
                        admission_proof.record(), sort_keys=True
                    )
                }
                if admission_proof is not None
                else {}
            ),
        }
        admitted_job = CodingJob(job_name, "setting-up", coding_metadata, None, None, "", "")
        acceptance_items = compile_acceptance_checklist(
            coding_task.prompt,
            contract,
            review_commands=rendered_review_commands(contract, admitted_job),
        )
        store.create_coding_job_with_acceptance(
            job_name=job_name,
            metadata=coding_metadata,
            goal=goal,
            items=acceptance_items,
            admission_attempt_id=(
                admission_proof.approval_attempt_id
                if admission_proof is not None
                else None
            ),
        )
        reservation_committed = True

    try:
        remote_branch = (
            create_git_backed_job_branch_or_exit(
                target,
                job_branch,
                before_create=reserve_coding_job,
            )
            if admission_proof is None
            else create_admitted_git_backed_job_branch_or_exit(
                target,
                job_branch,
                admission_proof,
                before_create=reserve_coding_job,
            )
        )
        store.set_metadata_value(job_name, "github_remote_branch_status", "created")
    except typer.Exit:
        if store.get_coding_job(job_name) is not None:
            store.update_status(job_name, "setup-failed")
            typer.echo(
                f"Retry this exact setup with --resume {job_name}",
                err=True,
            )
        raise
    except Exception as error:
        if reservation_committed:
            store.update_status(job_name, "setup-failed")
            typer.echo(
                f"Could not complete coding Job branch setup: {error}", err=True
            )
            typer.echo(
                f"Retry this exact setup with --resume {job_name}",
                err=True,
            )
            raise typer.Exit(1) from error
        if admission_proof is not None and admission_proof.approval_attempt_id:
            attempt = store.get_coding_admission(admission_proof.approval_attempt_id)
            if attempt is not None and attempt.status == "admitted":
                typer.echo(
                    f"This delegation was already admitted as coding Job {attempt.job_name}."
                )
                raise typer.Exit(0) from error
        typer.echo(f"Could not record coding Job: {error}", err=True)
        raise typer.Exit(1) from error

    try:
        worker = dorf.spawn_worker(
            worker_name,
            provenance="coding-workflow",
            lifecycle_policy="dedicated",
            room_metadata=room_image_metadata,
        )
        assignment = dorf.assign_job(
            job_name,
            worker_name=worker.worker.name,
            goal=goal,
            model=config.model,
            reasoning_effort=config.reasoning_effort,
            activate=False,
        )
        binding = assignment.binding
        if admission_proof is not None:
            store.documents.append_event(
                job_name,
                event_id=f"evt-{admission_proof.proof_id}",
                source="workflow",
                provenance="fact",
                kind="admission-proof",
                summary=f"Coding admission proved by {admission_proof.proof_id}",
                related={
                    "assignment": binding.assignment.id,
                    "proof": admission_proof.proof_id,
                    "worker": binding.worker.name,
                },
            )
    except Exception as error:
        store.update_status(job_name, "setup-failed")
        typer.echo(f"Could not create coding Worker and Job {job_name}: {error}", err=True)
        typer.echo(
            f"Retry this exact setup with --resume {job_name}",
            err=True,
        )
        raise typer.Exit(1) from error
    store.remove_metadata_keys(
        job_name,
        {
            "setup_model",
            "setup_reasoning_effort",
            "setup_task_prompt",
            "setup_provider_connection",
            "setup_image_fingerprint",
        },
    )

    try:
        execution = dorf.job_execution(job_name)
        prepare_git_workspace(
            execution,
            binding,
            repo_full_name=remote_branch.repo_full_name,
            token=remote_branch.token,
            branch=job_branch,
            git_author=git_author,
        )
        run_repository_preparation_or_raise(store, execution, binding, contract)
        activation = dorf.activate_job(job_name)
        binding = activation.binding
    except Exception as error:
        if store.get_coding_job(job_name) is not None:
            store.update_status(job_name, "setup-failed")
        typer.echo(f"Could not prepare coding Job {job_name}: {error}", err=True)
        typer.echo(
            f"Retry this exact Worker, Job, and Assignment with --resume {job_name}",
            err=True,
        )
        raise typer.Exit(1) from error

    store.update_status(job_name, "active")
    delivery_started = activation.dispatcher_started
    typer.echo(f"Started coding Job {job_name}")
    typer.echo(f"Worker: {worker_name} (coding-workflow, dedicated)")
    typer.echo(f"Assignment: {binding.assignment.id}")
    typer.echo(f"Workspace: {binding.workspace}")
    typer.echo(f"Branch: {job_branch}")
    checklist = store.get_acceptance_checklist(job_name)
    if checklist is not None:
        typer.echo(
            f"Acceptance: {checklist.state} ({len(checklist.items)} items; "
            f"correct with dorf acceptance {job_name} --from-file FILE before verify)"
        )
    typer.echo(
        "Initial goal delivery started detached"
        if delivery_started
        else "Initial goal delivery pending"
    )
    echo_contract_summary(contract)
    return job_name


def run_repository_preparation_or_raise(
    store: CodingStore,
    environment,
    binding: JobBinding,
    contract: RepoContract,
) -> None:
    job = store.get_coding_job(binding.job.name)
    if job is None:
        raise RuntimeError(f"CodingJob not found: {binding.job.name}")
    started = monotonic()
    run = prepare_coding_repository(store, environment, job, binding, contract)
    if run is None:
        return
    elapsed = monotonic() - started
    typer.echo(f"Repository preparation: {run.command} ({elapsed:.1f}s)")
    if run.exit_code != 0:
        raise RuntimeError(f"repository preparation exited with code {run.exit_code}")


def recover_setup_failed_coding_job_or_exit(
    store: CodingStore,
    job: CodingJob,
    coding_task: CodingTask,
) -> None:
    """Rebuild a failed coding clone without replacing Job or Worker identity."""
    repo = Path(job.target_repo)
    if not repo.exists():
        typer.echo(f"Coding Job host repo not found: {repo}", err=True)
        raise typer.Exit(1)
    contract = load_contract_or_exit(repo)
    worker_name = f"coder-{job.job_name}"
    binding = store.get_job_binding(job.job_name)
    worker_binding = store.get_worker_binding(worker_name)
    setup_provider_connection = job.metadata.get("setup_provider_connection")
    environment_config = IncusConfig.from_mapping(
        worker_binding.metadata if worker_binding is not None else contract.incus_config
    )
    if worker_binding is None and "setup_image_fingerprint" in job.metadata:
        environment_config = replace(
            environment_config,
            template=job.metadata["setup_image_fingerprint"],
        )
    dorf = Dorf(
        store,
        environment_config=environment_config,
        agent_defaults=contract.primary_codex,
        provider_connection=(
            worker_binding.metadata.get("provider_connection")
            if worker_binding is not None
            else setup_provider_connection
            or _missing_setup_provider_connection(job.job_name)
        ),
        git_credential_token=github_installation_token_for_job,
    )
    exit_if_environment_prerequisites_missing(dorf)
    workspace = f"/workspace/jobs/{job.job_name}"
    setup_task_prompt = job.metadata.get("setup_task_prompt")
    if setup_task_prompt is not None and setup_task_prompt != coding_task.prompt:
        typer.echo("Recorded coding setup intent no longer matches task context", err=True)
        raise typer.Exit(1)
    expected_goal = coding_job_goal(
        job_name=job.job_name,
        task=coding_task.prompt,
        job_branch=job.job_branch,
        workspace=workspace,
    )
    store.update_status(job.job_name, "setting-up")
    try:
        config = resolve_codex_config(
            (
                CodexConfig(
                    binding.conversation.model,
                    binding.conversation.reasoning_effort,
                )
                if binding is not None
                else contract.primary_codex
            ),
            model=job.metadata.get("setup_model") if binding is None else None,
            reasoning_effort=(
                job.metadata.get("setup_reasoning_effort") if binding is None else None
            ),
        )
        if worker_binding is None:
            worker_binding = dorf.spawn_worker(
                worker_name,
                provenance="coding-workflow",
                lifecycle_policy="dedicated",
                room_metadata=(
                    {"image_fingerprint": job.metadata["setup_image_fingerprint"]}
                    if "setup_image_fingerprint" in job.metadata
                    else None
                ),
            )
        if (
            worker_binding.worker.provenance != "coding-workflow"
            or worker_binding.worker.lifecycle_policy != "dedicated"
        ):
            raise RuntimeError("recorded Worker does not have dedicated coding provenance")
        assignment = dorf.assign_job(
            job.job_name,
            worker_name=worker_binding.worker.name,
            goal=expected_goal,
            model=config.model,
            reasoning_effort=config.reasoning_effort,
            activate=False,
        )
        binding = assignment.binding
        store.remove_metadata_keys(
            job.job_name,
            {
                "setup_model",
                "setup_reasoning_effort",
                "setup_task_prompt",
                "setup_image_fingerprint",
            },
        )
        remote_branch = recover_git_backed_job_branch_or_exit(job)
        store.set_metadata_value(job.job_name, "github_remote_branch_status", "created")
        execution = dorf.job_execution(job.job_name)
        reset_git_workspace(execution, binding)
        prepare_git_workspace(
            execution,
            binding,
            repo_full_name=remote_branch.repo_full_name,
            token=remote_branch.token,
            branch=job.job_branch,
            git_author=resolve_git_author_or_exit(repo),
        )
        run_repository_preparation_or_raise(store, execution, binding, contract)
        binding = dorf.activate_job(job.job_name).binding
    except Exception as error:
        store.update_status(job.job_name, "setup-failed")
        typer.echo(f"Could not recover coding Job setup: {error}", err=True)
        raise typer.Exit(1) from error
    store.update_status(job.job_name, "active")
    typer.echo(f"Recovered coding Job setup {job.job_name}")


def resolve_git_author_or_exit(repo: Path) -> GitAuthorIdentity:
    values: dict[str, str] = {}
    for key, label in (("user.name", "name"), ("user.email", "email")):
        result = run_git_unchecked(repo, "config", "--get", key)
        value = result.stdout.rstrip("\r\n") if result.returncode == 0 else ""
        if not value.strip():
            example = "Your Name" if key == "user.name" else "you@example.com"
            typer.echo(
                f"Git author {label} is missing or empty for {repo}. Configure it with: "
                f"git -C {shlex.quote(str(repo))} config --local {key} "
                f"{shlex.quote(example)}",
                err=True,
            )
            raise typer.Exit(1)
        values[key] = value
    return GitAuthorIdentity(name=values["user.name"], email=values["user.email"])


def resolve_coding_task_or_exit(
    repo: Path,
    *,
    task: str | None,
    issue_number: int | None,
) -> CodingTask:
    if (task is None) == (issue_number is None):
        typer.echo("Provide exactly one of TASK or --issue.", err=True)
        raise typer.Exit(1)
    if task is not None:
        return CodingTask(summary=task, prompt=task)

    repo_full_name = github_repo_full_name_or_exit(repo)
    try:
        issue = github_repository_client_from_app_token().get_issue(
            repo_full_name,
            issue_number,
        )
    except (GitHubAppConfigError, GitHubAppVerificationError, GitHubRepositoryError) as error:
        typer.echo(
            f"Could not retrieve GitHub issue #{issue_number}: {error}. "
            "Ensure the GitHub App installation has approved Issues: read permission.",
            err=True,
        )
        raise typer.Exit(1) from error
    return github_issue_task(repo_full_name, issue)


def prove_coding_admission_or_exit(
    target: GitTarget,
    *,
    command: str,
    issue_number: int,
    model: str | None,
    reasoning_effort: str | None,
    provider_connection: str | None,
) -> CodingAdmissionProof:
    """Run the AFK delegation's single proof before any workflow mutation."""
    request = CodingAdmissionRequest(
        repo_path=str(target.repo),
        target_branch=target.branch,
        issue_number=issue_number,
        command=command,
        target_start_sha=target.start_sha,
        provider_connection=provider_connection,
        model=model,
        reasoning_effort=reasoning_effort,
    )
    result = CodingAdmissionPreflight().prove(request)
    if result.proof is not None:
        proof = result.proof
        store = CodingStore.open()
        retained = store.get_coding_admission_for_request(request.record())
        if retained is not None and retained.status in {
            "pending",
            "approved",
            "admitted",
        }:
            retained_repository = retained.request.get("repository")
            if retained_repository != proof.repository:
                typer.echo(
                    "Retained GitHub authority is pinned to "
                    f"{retained_repository or 'an unknown repository'}, but the current "
                    f"checkout resolves to {proof.repository}. Restore the original repository "
                    "or make a new delegation.",
                    err=True,
                )
                raise typer.Exit(1)
            retained_installation = retained.request.get("installation_id")
            if retained_installation != proof.installation_id:
                typer.echo(
                    "Retained GitHub authority is pinned to installation "
                    f"{retained_installation or 'unknown'}, but readiness used installation "
                    f"{proof.installation_id}. Restore the original installation or make a new "
                    "delegation.",
                    err=True,
                )
                raise typer.Exit(1)
            if retained.status == "admitted":
                typer.echo(
                    "This delegation was already admitted as coding Job "
                    f"{retained.job_name}."
                )
                raise typer.Exit(0)
            if store.approve_coding_admission(retained.id):
                proof = replace(proof, approval_attempt_id=retained.id)
        typer.echo(f"Coding admission ready: {proof.proof_id}")
        return proof
    resumable = [
        failure
        for failure in result.failures
        if failure.automatic_continuation and failure.approval is not None
    ]
    if len(result.failures) == 1 and len(resumable) == 1:
        failure = resumable[0]
        assert failure.approval is not None
        pinned_request = replace(
            request,
            repository=failure.approval.repository,
            installation_id=failure.approval.installation_id,
        )
        store = CodingStore.open()
        retained = store.get_coding_admission_for_request(request.record())
        if (
            retained is not None
            and retained.status in {"pending", "approved", "admitted"}
            and (
                retained.request.get("repository") != failure.approval.repository
                or retained.request.get("installation_id")
                != failure.approval.installation_id
            )
        ):
            typer.echo(
                "This delegation already has retained GitHub authority for "
                f"{retained.request.get('repository') or 'an unknown repository'}; refusing "
                f"to retarget it to {failure.approval.repository} through installation "
                f"{failure.approval.installation_id}.",
                err=True,
            )
            raise typer.Exit(1)
        attempt, created = store.retain_pending_coding_admission(
            pinned_request.record(),
            failure.approval.record(),
            ttl_seconds=GITHUB_AUTHORITY_APPROVAL_TTL_SECONDS,
        )
        if attempt.status == "admitted":
            typer.echo(
                f"This delegation was already admitted as coding Job {attempt.job_name}."
            )
            raise typer.Exit(0)
        if attempt.status in {"declined", "expired"}:
            typer.echo(
                f"This GitHub authority approval attempt already ended: {attempt.status}.",
                err=True,
            )
            raise typer.Exit(1)
        if attempt.status == "pending":
            echo_github_authority_attention(attempt, retried=not created)
            outcome = await_github_authority_approval(attempt)
            if outcome == "installation-changed":
                typer.echo(
                    "Configured GitHub App installation changed while approval was pending. "
                    f"Restore installation {attempt.request['installation_id']} and retry.",
                    err=True,
                )
                raise typer.Exit(1)
            if outcome in {"declined", "expired"}:
                store.end_pending_coding_admission(attempt.id, outcome)
                typer.echo(f"GitHub authority approval {outcome}.", err=True)
                typer.echo(failure.approval.decline_consequence, err=True)
                raise typer.Exit(1)
            if not store.approve_coding_admission(attempt.id):
                typer.echo("GitHub authority approval expired.", err=True)
                raise typer.Exit(1)
        else:
            typer.echo("Reusing approved GitHub authority for this delegation.")
        typer.echo("GitHub authority approved; rerunning exact coding readiness.")
        resumed = CodingAdmissionPreflight().prove(pinned_request)
        if resumed.proof is not None:
            if resumed.proof.installation_id != attempt.request.get("installation_id"):
                typer.echo(
                    "Coding readiness used a different GitHub App installation than the "
                    "retained approval.",
                    err=True,
                )
                raise typer.Exit(1)
            if not store.approve_coding_admission(attempt.id):
                typer.echo("GitHub authority approval expired during readiness.", err=True)
                raise typer.Exit(1)
            proof = replace(resumed.proof, approval_attempt_id=attempt.id)
            typer.echo(f"Coding admission ready: {proof.proof_id}")
            return proof
        _echo_coding_admission_failures(resumed.failures)
        raise typer.Exit(1)
    _echo_coding_admission_failures(result.failures)
    raise typer.Exit(1)


def echo_github_authority_attention(
    attempt: PendingCodingAdmission, *, retried: bool
) -> None:
    attention = attempt.attention
    typer.echo("Attention: GitHub authority approval required")
    typer.echo(f"Attempt: {attempt.id}" + (" (retained)" if retried else ""))
    typer.echo(f"Missing authority: {attention['missing_authority']}")
    typer.echo(f"Why needed: {attention['why_needed']}")
    typer.echo(f"Approval action: {attention['action']}")
    typer.echo(f"Approval URL: {attention['url']}")
    typer.echo(f"Scope: {attention['scope']}")
    typer.echo(f"If approved: {attention['approve_consequence']}")
    typer.echo(f"If declined or expired: {attention['decline_consequence']}")
    typer.echo(f"Automatic resume: {attention['automatic_resume']}")
    typer.echo(f"Expires: {attempt.expires_at}")


def await_github_authority_approval(attempt: PendingCodingAdmission) -> str:
    """Open the one scoped GitHub action and observe authority without storing credentials."""
    webbrowser.open(attempt.attention["url"])
    repo = str(attempt.request["repository"])
    branch = str(attempt.request["target_branch"])
    installation_id = str(attempt.request["installation_id"])
    expires_at = datetime.fromisoformat(attempt.expires_at)
    try:
        while datetime.now(UTC) < expires_at:
            try:
                config = load_github_app_config()
                if config.installation_id != installation_id:
                    return "installation-changed"
                minted = GitHubAppTokenClient().mint_installation_token(config)
                token = (
                    minted.token
                    if isinstance(minted, GitHubInstallationToken)
                    else str(minted)
                )
                GitHubRepositoryClient(token).get_branch_sha(repo, branch)
                return "approved"
            except (
                GitHubAppConfigError,
                GitHubAppVerificationError,
                GitHubRepositoryError,
            ):
                sleep(GITHUB_AUTHORITY_POLL_SECONDS)
    except KeyboardInterrupt:
        return "declined"
    return "expired"


def _echo_coding_admission_failures(failures) -> None:
    typer.echo(
        "Coding admission failed with "
        f"{len(failures)} independently discovered failure(s):",
        err=True,
    )
    for failure in failures:
        typer.echo(f"- [{failure.code}] {failure.summary}", err=True)
        typer.echo(f"  owner: {failure.owner}", err=True)
        typer.echo(f"  repair: {failure.repair}", err=True)
        typer.echo(f"  consequence: {failure.consequence}", err=True)
        typer.echo(
            "  automatic continuation: "
            + ("yes" if failure.automatic_continuation else "no"),
            err=True,
        )


def github_issue_task(repo_full_name: str, issue: GitHubIssue) -> CodingTask:
    summary = f"Issue #{issue.number}: {issue.title}"
    sections = [
        summary,
        f"Repository: {repo_full_name}",
        "",
        "Issue body:",
        issue.body or "(No issue body provided.)",
    ]
    if issue.comments:
        sections.extend(["", "Issue comments:"])
        sections.extend(f"- {comment}" for comment in issue.comments)
    sections.extend(
        [
            "",
            "Implementation contract:",
            "- Use only the Assignment workspace named in the coding Job goal.",
            "- Keep the change scoped to this issue and follow the repository instructions.",
            "- Use TDD. Run the repository's configured verification before finalizing.",
            "- Commit the completed change and push the assigned Job branch to origin.",
            "- Finish with a PR-ready response containing status, commit, draft PR title "
            "and body, verification, and notes.",
        ]
    )
    return CodingTask(summary=summary, prompt="\n".join(sections))


@app.command()
def doctor() -> None:
    """Diagnose the configured core Provider Gateway and Incus Room path."""
    with ProviderGateway.open() as gateway:
        gateway_health = gateway.health()
    typer.echo(f"provider-gateway: {gateway_health.status}")
    backend_presence = "present" if gateway_health.backend_present else "missing"
    typer.echo(f"provider-backend: {backend_presence} (pinned {gateway_health.backend_version})")
    typer.echo(f"provider-bind: {', '.join(gateway_health.bind_addresses)}")
    connections = "connected" if gateway_health.has_provider_connection else "none"
    typer.echo(f"provider-connections: {connections}")

    try:
        profile = load_optional_deployment_profile()
    except DeploymentProfileError as error:
        echo_diagnostic_stop(
            "Doctor",
            SetupDiagnostic(
                status="failed",
                owner="dorf",
                classification="configuration",
                summary=f"Global Dorf configuration is invalid: {error}",
                observed=(str(error),),
                expected=("The global deployment profile is valid and readable.",),
                safe_actions=("Repair or remove the reported deployment.json.",),
                reproducer=("dorf doctor",),
            ),
            resume_command="dorf doctor",
        )
        raise typer.Exit(1) from error
    config = profile.incus if profile is not None else IncusConfig()
    typer.echo(
        "deployment-profile: "
        + ("configured" if profile is not None else "not configured; checking defaults")
    )
    result = IncusDoctor().core_check(config)
    if result.ok:
        typer.echo("incus-vm: ok")
        return
    observed = tuple(f"{failure.code}: {failure.message}" for failure in result.failures)
    echo_diagnostic_stop(
        "Doctor",
        SetupDiagnostic(
            status="failed",
            owner="incus",
            classification="configuration",
            summary="The configured Incus Room path is not ready.",
            observed=observed,
            expected=(
                "Incus can launch the configured VM image on the private network "
                "with working guest-agent and outbound connectivity.",
            ),
            safe_actions=("Run dorf setup to apply reviewed remediation.",),
            reproducer=("dorf doctor",),
        ),
        resume_command="dorf doctor",
    )
    raise typer.Exit(1)


@app.command()
def init() -> None:
    """Create a minimal repo contract."""
    target = detect_git_target(Path.cwd())
    contract_path = target.repo / CONTRACT_FILENAME
    if contract_path.exists():
        typer.echo(f"{CONTRACT_FILENAME} already exists.", err=True)
        raise typer.Exit(1)

    contract_path.write_text("[commands]\n")
    typer.echo(f"Created {CONTRACT_FILENAME}")


@github_app.command("setup")
def github_setup(
    org: str | None = typer.Option(
        None,
        "--org",
        help="Create the GitHub App under this organization instead of your personal account.",
    ),
    host: str = typer.Option("127.0.0.1", "--host", help="Local callback host."),
    port: int = typer.Option(0, "--port", help="Local callback port. Use 0 for a free port."),
) -> None:
    """Create, install, store, and verify the Dorf GitHub App."""
    try:
        result = GitHubAppManifestFlow(host=host, port=port, org=org).run(announce=typer.echo)
    except GitHubAppManifestFlowError as error:
        typer.echo(f"github: setup failed ({error})", err=True)
        raise typer.Exit(1) from error
    typer.echo(f"github: configured app {result.config.app_id}")
    typer.echo(f"github: installation {result.config.installation_id}")
    typer.echo(f"Stored GitHub App metadata: {result.paths.metadata_path}")
    typer.echo(f"Stored GitHub App private key: {result.paths.private_key_path}")
    typer.echo("github: verified")


@github_app.command("status")
def github_status(
    verify: bool = typer.Option(
        False,
        "--verify",
        help="Mint an installation token to verify GitHub App credentials.",
    ),
) -> None:
    """Show GitHub App setup status."""
    try:
        config = load_github_app_config()
    except GitHubAppConfigError as error:
        typer.echo(f"github: not configured ({error})", err=True)
        raise typer.Exit(1) from error

    paths = github_app_paths()
    typer.echo("github: configured")
    typer.echo(f"App ID: {config.app_id}")
    typer.echo(f"Installation ID: {config.installation_id}")
    if config.app_slug:
        typer.echo(f"App slug: {config.app_slug}")
    typer.echo(f"Metadata: {paths.metadata_path}")
    typer.echo(f"Private key: {paths.private_key_path}")
    if not private_key_permissions_are_locked_down():
        typer.echo("GitHub App private key permissions are too broad.", err=True)
        raise typer.Exit(1)
    if not verify:
        return
    try:
        GitHubAppTokenClient().mint_installation_token(config)
    except GitHubAppVerificationError as error:
        typer.echo(f"github: verification failed ({error})", err=True)
        raise typer.Exit(1) from error
    typer.echo("github: verified")


def echo_job_timeline(name: str, events: list[TimelineEvent]) -> None:
    typer.echo(f"Timeline · {name}")
    if not events:
        typer.echo("No timeline entries recorded.")
        return
    for event in events:
        observed = f"{event.recorded_at[:19].replace('T', ' ')}Z"
        typer.echo(
            f"{event.sequence:04d} {observed} [{event.source} {event.provenance}] {event.summary}"
        )
        echo_event_related(event)
        for artifact in event.artifacts:
            typer.echo(f"     artifact: {artifact.name} · {artifact.path}")


def echo_job_evidence(
    name: str,
    events: list[TimelineEvent],
    job_path: Path,
) -> None:
    typer.echo(f"Evidence · {name}")
    entries = [event for event in events if event.artifacts]
    if not entries:
        typer.echo("No evidence recorded.")
        return
    for event in entries:
        typer.echo(f"{event.sequence:04d} [{event.source} {event.provenance}] {event.summary}")
        if event.source == "worker" and event.provenance == "claim":
            typer.echo("     Worker claim; content has not been independently verified")
        echo_event_related(event)
        for artifact in event.artifacts:
            typer.echo(f"     {artifact.name} · {artifact.media_type} · {artifact.size} bytes")
            typer.echo(f"     {artifact.digest} · {job_path / artifact.path}")


def echo_job_artifact(artifact: JobArtifact) -> None:
    typer.echo(f"{artifact.name} · {artifact.media_type} · {artifact.size} bytes")
    typer.echo(f"     ref: {artifact.ref}")
    typer.echo(f"     digest: {artifact.digest}")
    typer.echo(f"     event: {artifact.event_id} · {artifact.source} {artifact.provenance}")


def echo_event_related(event: TimelineEvent) -> None:
    if not event.related:
        return
    related = " ".join(f"{key}={value}" for key, value in sorted(event.related.items()))
    typer.echo(f"     related: {related}")


@app.command()
def status(job_name: str) -> None:
    """Show coding workflow state alongside its current Assignment binding."""
    store = CodingStore.open()
    job = store.get_coding_job(job_name)
    binding = store.get_job_binding(job_name)
    if job is None or binding is None:
        typer.echo(f"Coding Job not found: {job_name}", err=True)
        raise typer.Exit(1)
    typer.echo(f"Job: {job.job_name} ({job.status})")
    typer.echo(f"Task: {job.task}")
    typer.echo(f"Worker: {binding.worker.name}")
    typer.echo(f"Worker provenance: {binding.worker.provenance}")
    typer.echo(f"Worker lifecycle policy: {binding.worker.lifecycle_policy}")
    typer.echo(f"Assignment: {binding.assignment.id} ({binding.assignment.status})")
    typer.echo(f"Room: {binding.room.provider_id} ({binding.room.status})")
    typer.echo(f"Workspace: {binding.workspace}")
    typer.echo(f"Target: {job.target_branch} at {job.target_start_sha}")
    typer.echo(f"Branch: {job.job_branch}")
    typer.echo(
        f"Conversation: {binding.conversation.status} "
        f"({binding.conversation.native_conversation_id or 'not started'})"
    )
    if job.github_pr_number is not None and job.github_pr_url:
        typer.echo(f"GitHub PR: #{job.github_pr_number} {job.github_pr_url}")
    if stage := job.metadata.get("afk_stage"):
        typer.echo(f"AFK: {stage} ({job.metadata.get('afk_outcome', 'pending')})")


@app.command()
def acceptance(
    job_name: str,
    from_file: Path | None = ACCEPTANCE_FILE_OPTION,
    json_output: bool = typer.Option(False, "--json", help="Emit agent-readable JSON."),
) -> None:
    """Inspect or correct the admission checklist before verification governs completion."""
    store = CodingStore.open()
    job = store.get_coding_job(job_name)
    checklist = store.get_acceptance_checklist(job_name)
    if job is None or checklist is None:
        typer.echo(f"Acceptance checklist not found: {job_name}", err=True)
        raise typer.Exit(1)
    if from_file is not None:
        if not from_file.is_file():
            typer.echo(f"Acceptance checklist file not found: {from_file}", err=True)
            raise typer.Exit(1)
        contract = load_contract_or_exit(Path(job.target_repo))
        compiled = compile_acceptance_checklist(
            from_file.read_text(),
            contract,
            review_commands=rendered_review_commands(contract, job),
        )
        corrected = tuple(
            replace(item, source="human")
            if item.source in {"goal", "issue"}
            else item
            for item in compiled
        )
        try:
            checklist = store.replace_acceptance_checklist(job_name, corrected)
        except (RuntimeError, ValueError) as error:
            typer.echo(str(error), err=True)
            raise typer.Exit(1) from error
    if json_output:
        typer.echo(json.dumps(asdict(checklist), sort_keys=True))
        return
    typer.echo(
        f"Acceptance · {job_name} · {checklist.state} revision {checklist.revision} · "
        f"goal {checklist.goal_digest}"
    )
    for item in checklist.items:
        verifier_ref = f"={item.verifier_ref}" if item.verifier_ref else ""
        typer.echo(
            f"- [ ] {item.text} [{item.source}; {item.verifier}{verifier_ref}]"
        )


@app.command()
def dossier(
    job_name: str,
    commit: str | None = typer.Option(
        None,
        "--commit",
        help="Assess this exact commit instead of the latest retained proof commit.",
    ),
    json_output: bool = typer.Option(False, "--json", help="Emit agent-readable JSON."),
) -> None:
    """Show the compact, commit-pinned coding proof dossier."""
    store = CodingStore.open()
    job = store.get_coding_job(job_name)
    binding = store.get_job_binding(job_name)
    if job is None or binding is None:
        typer.echo(f"Coding Job not found: {job_name}", err=True)
        raise typer.Exit(1)
    commit_sha = commit or proof_dossier_commit(store, job)
    if not re.fullmatch(r"[0-9a-f]{40}", commit_sha):
        typer.echo(f"Invalid dossier commit: {commit_sha}", err=True)
        raise typer.Exit(1)
    proof = build_proof_dossier(store, job, binding, commit_sha=commit_sha)
    if json_output:
        typer.echo(json.dumps(asdict(proof), sort_keys=True))
        return
    typer.echo(render_proof_dossier(proof))


@app.command("implementation-status")
def implementation_status(
    job_name: str,
    wait: bool = typer.Option(False, "--wait", help="Wait for implementation to finish."),
) -> None:
    """Show the initial goal turn used for coding implementation."""
    store = CodingStore.open()
    while True:
        job = store.get_coding_job(job_name)
        inputs = store.list_job_inputs(job_name) if job is not None else []
        if job is None or not inputs or inputs[0].kind != "goal":
            typer.echo(f"Coding Job implementation not found: {job_name}", err=True)
            raise typer.Exit(1)
        turn = store.get_job_turn_by_input(job_name, inputs[0].id)
        if turn is not None and (not wait or turn.status != "running"):
            typer.echo(f"Implementation: {turn.status}")
            typer.echo(f"Turn ID: {turn.id}")
            if turn.exit_code is not None:
                typer.echo(f"Exit code: {turn.exit_code}")
            typer.echo(f"Output: {turn.output_path}")
            return
        if not wait:
            typer.echo("Implementation: queued")
            typer.echo(f"Input: {inputs[0].id}")
            return
        sleep(0.25)


@app.command()
def ready(job_name: str) -> None:
    """Mark a coding Job ready after mechanical checks."""
    run_coding_job_workflow_or_exit(
        job_name, lambda workflow: workflow.mark_ready(), require_runnable=False
    )


@app.command()
def bootstrap(job_name: str) -> None:
    """Run the configured bootstrap command in a coding Job workspace."""
    run_configured_job_command(job_name, "bootstrap")


@app.command()
def check(job_name: str) -> None:
    """Run the configured check command in a coding Job workspace."""
    run_configured_job_command(job_name, "check")


@app.command()
def smoke(job_name: str) -> None:
    """Run the configured smoke command in a coding Job workspace."""
    run_configured_job_command(job_name, "smoke")


def coding_job_workflow_or_exit(
    job_name: str,
    *,
    require_runnable: bool = True,
    require_execution: bool = True,
) -> CodingWorkflow:
    store = CodingStore.open()
    job = (
        get_runnable_coding_job_or_exit(store, job_name)
        if require_runnable
        else store.get_coding_job(job_name)
    )
    if job is None:
        typer.echo(f"Coding Job not found: {job_name}", err=True)
        raise typer.Exit(1)
    binding = store.get_job_binding(job_name)
    if binding is None:
        typer.echo(f"Job binding not found: {job_name}", err=True)
        raise typer.Exit(1)
    contract = load_contract_or_exit(Path(job.target_repo))
    execution = None
    if require_execution:
        execution = job_execution_or_exit(store, job_name)
    return CodingWorkflow(
        store=store,
        job=job,
        contract=contract,
        execution=execution,
        github_client=github_repository_client_from_app_token,
        github_app_slug=github_app_slug,
    )


def github_app_slug() -> str:
    try:
        config = load_github_app_config()
    except GitHubAppConfigError as error:
        raise RuntimeError(f"GitHub App metadata could not be loaded: {error}") from error
    if not config.app_slug:
        raise RuntimeError("GitHub App metadata is missing app_slug; rerun dorf github setup.")
    return config.app_slug


def echo_workflow_outcome(outcome: WorkflowOutcome) -> None:
    for message in outcome.messages:
        typer.echo(message.text, err=message.error)


def run_coding_job_workflow_or_exit(
    job_name: str,
    operation: Callable[[CodingWorkflow], WorkflowOutcome],
    *,
    require_runnable: bool = True,
    require_execution: bool = True,
) -> None:
    workflow = coding_job_workflow_or_exit(
        job_name,
        require_runnable=require_runnable,
        require_execution=require_execution,
    )
    try:
        outcome = operation(workflow)
    except WorkflowFailure as error:
        echo_workflow_outcome(WorkflowOutcome(error.messages, error.exit_code))
        raise typer.Exit(error.exit_code) from error
    echo_workflow_outcome(outcome)


@app.command()
def review(job_name: str, agents: list[str] | None = REVIEW_AGENT_OPTION) -> None:
    """Run configured one-shot review commands for a coding Job."""
    run_coding_job_workflow_or_exit(job_name, lambda workflow: workflow.review(agents))


@app.command()
def verify(job_name: str, agents: list[str] | None = REVIEW_AGENT_OPTION) -> None:
    """Run bounded check, review, and Job-message repair rounds."""
    run_coding_job_workflow_or_exit(job_name, lambda workflow: workflow.verify(agents))


@app.command("verify-role")
def verify_role(job_name: str, role: str) -> None:
    """Run the concrete DeepSeek diff role as a disposable shadow review."""
    if role != "diff":
        typer.echo("Only the diff verifier role is supported.", err=True)
        raise typer.Exit(2)
    store = CodingStore.open()
    job = get_runnable_coding_job_or_exit(store, job_name)
    contract = load_contract_or_exit(Path(job.target_repo))
    _, profile = select_coding_deployment_or_exit("deepseek")
    config = IncusConfig.from_mapping(contract.incus_config)
    fingerprint = deployment_image_fingerprint(profile, contract)
    if fingerprint is not None:
        config = replace(config, template=fingerprint)
    repo = job.metadata.get("github_repo", "")
    try:
        app_config = load_github_app_config()
        minted = GitHubAppTokenClient().mint_installation_token(
            app_config,
            repositories=[repo.rsplit("/", 1)[-1]],
            permissions={"contents": "read"},
        )
        gateway = Dorf.open_provider_gateway(config)
        dorf = Dorf(
            store,
            environment_config=config,
            provider_connection="deepseek",
            provider_gateway=gateway,
        )
        run = run_shadow_review(
            store,
            job,
            dorf,
            gateway,
            GitHubRepositoryClient(minted.token),
            minted.token,
        )
    except CommandInterrupted:
        raise typer.Exit(130) from None
    except (GitHubAppConfigError, GitHubAppVerificationError, RuntimeError) as error:
        typer.echo(f"Shadow verifier failed: {error}", err=True)
        raise typer.Exit(1) from error
    exit_for_command_run(job_name, run.kind, run.exit_code)


@app.command()
def followup(job_name: str) -> None:
    """Route linked PR feedback through the same Job conversation."""
    run_coding_job_workflow_or_exit(job_name, lambda workflow: workflow.followup())


@app.command("exec")
def exec_job_command(
    job_name: str,
    argv: list[str] = COMMAND_ARGV_ARGUMENT,
) -> None:
    """Run a non-interactive command in the assigned Job workspace."""
    if not argv:
        typer.echo("Missing command argv.", err=True)
        raise typer.Exit(1)
    store = CodingStore.open()
    job = get_runnable_coding_job_or_exit(store, job_name)
    contract = load_contract_or_exit(Path(job.target_repo))
    try:
        run = run_environment_job_command(
            store=store,
            job=job,
            contract=contract,
            spec=argv_command("exec", argv),
        )
    except CommandInterrupted:
        raise typer.Exit(130) from None
    exit_for_command_run(job_name, run.kind, run.exit_code)


@app.command()
def publish(job_name: str) -> None:
    """Create or update the GitHub PR for a verified coding Job."""
    run_coding_job_workflow_or_exit(
        job_name,
        lambda workflow: workflow.publish(),
        require_runnable=False,
        require_execution=False,
    )


@app.command()
def complete(job_name: str) -> None:
    """Record merged or rejected PR truth and end its runtime resources."""
    store = CodingStore.open()
    job = store.get_coding_job(job_name)
    if job is None:
        typer.echo(f"Coding Job not found: {job_name}", err=True)
        raise typer.Exit(1)
    if job.status in {"merged", "rejected"}:
        typer.echo(f"Coding Job already {job.status}: {job_name}")
        end_coding_resources_or_exit(job_name, interrupt=True)
        return
    if job.status == "abandoned":
        typer.echo(f"Abandoned coding Job cannot be completed: {job_name}", err=True)
        raise typer.Exit(1)
    if job.github_pr_number is None:
        typer.echo(f"Coding Job has no linked GitHub PR: {job_name}", err=True)
        raise typer.Exit(1)
    repo_full_name = job.metadata.get("github_repo")
    if not repo_full_name:
        typer.echo("Coding Job metadata is missing github_repo", err=True)
        raise typer.Exit(1)
    try:
        pr = github_repository_client_from_app_token().get_pull_request(
            repo_full_name, job.github_pr_number
        )
    except (GitHubAppConfigError, GitHubAppVerificationError, GitHubRepositoryError) as error:
        typer.echo(f"Could not inspect GitHub PR: {error}", err=True)
        raise typer.Exit(1) from error
    if pr.get("state") == "open":
        typer.echo(f"Linked GitHub PR is still open: #{job.github_pr_number}", err=True)
        raise typer.Exit(1)
    terminal = "merged" if pr.get("merged") is True else "rejected"
    store.update_status(job_name, terminal)
    typer.echo(f"Recorded coding Job {job_name}: {terminal}")
    end_coding_resources_or_exit(job_name, interrupt=True)


@app.command()
def runs(job_name: str) -> None:
    """List workflow-owned command runs for a coding Job."""
    store = CodingStore.open()
    if store.get_coding_job(job_name) is None:
        typer.echo(f"Coding Job not found: {job_name}", err=True)
        raise typer.Exit(1)
    command_runs = store.list_command_runs(job_name)
    if not command_runs:
        typer.echo("No workflow command runs.")
        return
    for run in command_runs:
        typer.echo(
            "  ".join(
                [
                    str(run.id),
                    run.kind,
                    run.status,
                    "" if run.exit_code is None else str(run.exit_code),
                    run.started_at,
                    run.finished_at or "",
                    run.command,
                ]
            )
        )


@app.command("show-run")
def show_run(job_name: str, run_id: int) -> None:
    """Print one workflow command's stored output."""
    store = CodingStore.open()
    run = store.get_command_run(run_id)
    if store.get_coding_job(job_name) is None or run is None or run.job_name != job_name:
        typer.echo(f"Run not found: {run_id}", err=True)
        raise typer.Exit(1)
    output_path = Path(run.output_path)
    if not run.output_path or not output_path.is_file():
        typer.echo(f"Run output not found: {run.output_path or run_id}", err=True)
        raise typer.Exit(1)
    typer.echo(output_path.read_text(), nl=False)


@app.command()
def discard(job_name: str) -> None:
    """Record explicit coding abandonment and end its runtime resources."""
    store = CodingStore.open()
    job = store.get_coding_job(job_name)
    if job is None:
        typer.echo(f"Coding Job not found: {job_name}", err=True)
        raise typer.Exit(1)
    if job.status in {"merged", "rejected"}:
        typer.echo(f"Terminal coding Job cannot be abandoned: {job_name} ({job.status})", err=True)
        raise typer.Exit(1)
    if job.status != "abandoned":
        store.update_status(job_name, "abandoned")
    typer.echo(f"Recorded coding Job {job_name}: abandoned")
    end_coding_resources_or_exit(job_name, interrupt=True)


def end_coding_resources_or_exit(job_name: str, *, interrupt: bool) -> None:
    store = CodingStore.open()
    binding = store.get_job_binding(job_name)
    if binding is None:
        raise typer.Exit(1)
    try:
        with Dorf(store) as dorf:
            dorf.end_job(job_name, interrupt=interrupt)
    except RuntimeError as error:
        typer.echo(f"Coding resource cleanup remains retryable: {error}", err=True)
        raise typer.Exit(1) from error
    typer.echo(f"Ended coding resources: {job_name}")


def run_configured_job_command(job_name: str, command_name: str) -> None:
    store = CodingStore.open()
    job = get_runnable_coding_job_or_exit(store, job_name)
    contract = load_contract_or_exit(Path(job.target_repo))
    command = contract.commands.get(command_name)
    if command is None:
        typer.echo(f"No configured {command_name} command for coding Job {job_name}.", err=True)
        raise typer.Exit(1)
    if command_name in {"check", "smoke"}:
        checklist = store.get_acceptance_checklist(job_name)
        if checklist is not None:
            store.freeze_acceptance_checklist(job_name)
    try:
        run = run_environment_job_command(
            store=store,
            job=job,
            contract=contract,
            spec=shell_command(command_name, command),
        )
    except CommandInterrupted:
        raise typer.Exit(130) from None
    exit_for_command_run(job_name, command_name, run.exit_code)


def rendered_review_commands(contract: RepoContract, job: CodingJob) -> dict[str, str]:
    review = contract.review
    if review is None:
        return {}
    return {
        name: review_command_with_dorf_protocol(agent.command, review.prompt, job=job)
        for name, agent in review.agents.items()
        if agent.enabled
    }


def validate_dorf_branch_or_exit(branch: str, *, target_branch: str) -> None:
    reason = unsafe_dorf_branch_reason(branch, target_branch=target_branch)
    if reason is None:
        return
    typer.echo(f"Refusing unsafe Dorf branch name: {branch} ({reason})", err=True)
    raise typer.Exit(1)


def unsafe_dorf_branch_reason(branch: str, *, target_branch: str) -> str | None:
    if branch in PROTECTED_BRANCHES:
        return "protected branch"
    if branch == target_branch:
        return "target branch"
    if not branch.startswith(DORF_BRANCH_PREFIX):
        return f"does not start with {DORF_BRANCH_PREFIX}"
    suffix = branch.removeprefix(DORF_BRANCH_PREFIX)
    if not suffix:
        return "missing Job id"
    if suffix in {".", ".."} or suffix.startswith("../") or "/../" in suffix:
        return "contains parent-directory traversal"
    if suffix.startswith("/") or suffix.endswith("/"):
        return "contains an empty path segment"
    if "//" in suffix:
        return "contains an empty path segment"
    if suffix.endswith(".lock"):
        return "ends with .lock"
    return None


def get_runnable_coding_job_or_exit(store: CodingStore, job_name: str) -> CodingJob:
    job = store.get_coding_job(job_name)
    if job is None:
        typer.echo(f"Coding Job not found: {job_name}", err=True)
        raise typer.Exit(1)
    if job.status in {"setup-failed", "abandoned", "merged", "rejected"}:
        typer.echo(
            f"Coding Job does not allow command execution: {job_name} ({job.status})",
            err=True,
        )
        raise typer.Exit(1)
    if not Path(job.target_repo).exists():
        typer.echo(f"Coding Job host repo not found: {job.target_repo}", err=True)
        raise typer.Exit(1)
    if store.get_job_binding(job_name) is None:
        typer.echo(f"Job binding not found: {job_name}", err=True)
        raise typer.Exit(1)
    return job


def exit_for_command_run(job_name: str, kind: str, exit_code: int | None) -> None:
    if exit_code is None:
        typer.echo(f"{kind} did not finish for {job_name}.", err=True)
        raise typer.Exit(1)
    if exit_code != 0:
        typer.echo(
            f"{kind} failed for {job_name} with exit code {exit_code}.",
            err=True,
        )
        raise typer.Exit(exit_code)
    typer.echo(f"{kind} succeeded for {job_name}")


def run_environment_job_command(
    store: CodingStore,
    job: CodingJob,
    contract: RepoContract,
    spec,
):
    binding = store.get_job_binding(job.job_name)
    if binding is None:
        typer.echo(f"Job binding not found: {job.job_name}", err=True)
        raise typer.Exit(1)
    execution = job_execution_or_exit(store, job.job_name)
    return run_coding_job_command(
        store=store,
        environment=execution,
        job=job,
        binding=binding,
        contract=contract,
        spec=spec,
    )


def job_execution_or_exit(store: CodingStore, job_name: str):
    try:
        dorf = Dorf(store, git_credential_token=github_installation_token_for_job)
        return dorf.job_execution(job_name)
    except (UnsupportedRoomTypeError, RuntimeError) as error:
        typer.echo(str(error), err=True)
        raise typer.Exit(1) from error


def github_installation_token_for_job(job_name: str) -> str:
    job = CodingStore.open().get_coding_job(job_name)
    if job is None:
        raise RuntimeError(f"Coding Job not found: {job_name}")
    repo_full_name = job.metadata.get("github_repo")
    if not repo_full_name:
        raise RuntimeError("Coding Job metadata is missing github_repo")
    try:
        config = load_github_app_config()
        minted = GitHubAppTokenClient().mint_installation_token(config)
    except (GitHubAppConfigError, GitHubAppVerificationError) as error:
        raise RuntimeError(f"could not refresh GitHub App token: {error}") from error
    return minted.token if isinstance(minted, GitHubInstallationToken) else str(minted)


def detect_git_target(cwd: Path) -> GitTarget:
    repo = Path(run_git(cwd, "rev-parse", "--show-toplevel"))
    branch = run_git(repo, "branch", "--show-current")
    if not branch:
        typer.echo("Target repo is in detached HEAD state.", err=True)
        raise typer.Exit(1)
    start_sha = run_git(repo, "rev-parse", "HEAD")
    return GitTarget(repo=repo, branch=branch, start_sha=start_sha)


def detect_git_repo_root(cwd: Path) -> Path | None:
    result = run_git_unchecked(cwd, "rev-parse", "--show-toplevel")
    if result.returncode != 0:
        return None
    return Path(result.stdout.strip())


def create_git_backed_job_branch_or_exit(
    target: GitTarget,
    job_branch: str,
    before_create: Callable[[GitBackedJobBranch], None] | None = None,
) -> GitBackedJobBranch:
    repo_full_name = github_repo_full_name_or_exit(target.repo)
    try:
        config = load_github_app_config()
        minted = GitHubAppTokenClient().mint_installation_token(config)
        token = minted.token if isinstance(minted, GitHubInstallationToken) else str(minted)
        expires_at = minted.expires_at if isinstance(minted, GitHubInstallationToken) else None
        client = GitHubRepositoryClient(token)
        base_sha = client.get_branch_sha(repo_full_name, target.branch)
        ensure_base_commit_exists_locally_or_exit(
            repo=target.repo,
            repo_full_name=repo_full_name,
            base_branch=target.branch,
            base_sha=base_sha,
            token=token,
        )
        branch = GitBackedJobBranch(
            repo_full_name=repo_full_name,
            base_sha=base_sha,
            metadata={
                "local_target_repo": str(target.repo),
                "github_repo": repo_full_name,
                "github_base_branch": target.branch,
                "github_remote_branch_status": "pending",
                **({"github_token_expires_at": expires_at} if expires_at else {}),
            },
            token=token,
        )
        if before_create is not None:
            before_create(branch)
        client.create_branch(repo_full_name, job_branch, base_sha)
    except GitHubAppConfigError as error:
        typer.echo(f"github: not configured ({error})", err=True)
        raise typer.Exit(1) from error
    except (GitHubAppVerificationError, GitHubRepositoryError) as error:
        typer.echo(f"Could not create remote Job branch: {error}", err=True)
        raise typer.Exit(1) from error

    return branch


def create_admitted_git_backed_job_branch_or_exit(
    target: GitTarget,
    job_branch: str,
    proof: CodingAdmissionProof,
    before_create: Callable[[GitBackedJobBranch], None] | None = None,
) -> GitBackedJobBranch:
    """Create only the branch already authorized by an exact admission proof."""
    if target.branch != proof.target_branch or target.start_sha != proof.target_start_sha:
        typer.echo("Coding admission proof no longer matches the target branch.", err=True)
        raise typer.Exit(1)
    branch = GitBackedJobBranch(
        repo_full_name=proof.repository,
        base_sha=proof.target_start_sha,
        metadata={
            "local_target_repo": str(target.repo),
            "github_repo": proof.repository,
            "github_base_branch": proof.target_branch,
            "github_remote_branch_status": "pending",
        },
        token=proof.installation_token,
    )
    try:
        client = GitHubRepositoryClient(proof.installation_token)
        current_target_sha = client.get_branch_sha(
            proof.repository,
            proof.target_branch,
        )
        if current_target_sha != proof.target_start_sha:
            typer.echo(
                "The target branch advanced after coding admission; repeat the delegation "
                "to prove its current head.",
                err=True,
            )
            raise typer.Exit(1)
        if before_create is not None:
            before_create(branch)
        client.create_branch(
            proof.repository,
            job_branch,
            proof.target_start_sha,
        )
    except GitHubRepositoryError as error:
        typer.echo(f"Could not create admitted remote Job branch: {error}", err=True)
        raise typer.Exit(1) from error
    return branch


def recover_git_backed_job_branch_or_exit(
    job: CodingJob,
) -> GitBackedJobBranch:
    validate_dorf_branch_or_exit(job.job_branch, target_branch=job.target_branch)
    repo_full_name = job.metadata.get("github_repo")
    if not repo_full_name:
        typer.echo(
            "Could not recover remote Job branch: Job metadata is missing github_repo.",
            err=True,
        )
        raise typer.Exit(1)
    try:
        config = load_github_app_config()
        minted = GitHubAppTokenClient().mint_installation_token(config)
        token = minted.token if isinstance(minted, GitHubInstallationToken) else str(minted)
        client = GitHubRepositoryClient(token)
        try:
            branch_sha = client.get_branch_sha(repo_full_name, job.job_branch)
        except GitHubRepositoryError as error:
            if not github_branch_not_found_error(error):
                raise
            client.create_branch(repo_full_name, job.job_branch, job.target_start_sha)
            branch_sha = job.target_start_sha
    except (GitHubAppConfigError, GitHubAppVerificationError, GitHubRepositoryError) as error:
        typer.echo(f"Could not recover remote Job branch: {error}", err=True)
        raise typer.Exit(1) from error
    if branch_sha != job.target_start_sha:
        typer.echo(
            f"Could not recover remote Job branch: {job.job_branch} moved from "
            f"the recorded target start SHA {job.target_start_sha} to {branch_sha}.",
            err=True,
        )
        raise typer.Exit(1)
    return GitBackedJobBranch(
        repo_full_name=repo_full_name,
        base_sha=job.target_start_sha,
        metadata={},
        token=token,
    )


def github_branch_not_found_error(error: Exception) -> bool:
    return "HTTP 404" in str(error)


def ensure_base_commit_exists_locally_or_exit(
    *,
    repo: Path,
    repo_full_name: str,
    base_branch: str,
    base_sha: str,
    token: str,
) -> None:
    if git_commit_exists(repo, base_sha):
        return
    fetch_github_branch_objects_or_exit(
        repo=repo,
        repo_full_name=repo_full_name,
        branch=base_branch,
        token=token,
    )
    if git_commit_exists(repo, base_sha):
        return
    typer.echo(
        f"Remote base SHA is not present locally after fetch: {base_sha}",
        err=True,
    )
    raise typer.Exit(1)


def git_commit_exists(repo: Path, sha: str) -> bool:
    result = run_git_unchecked(repo, "cat-file", "-e", f"{sha}^{{commit}}")
    return result.returncode == 0


def fetch_github_branch_objects_or_exit(
    *,
    repo: Path,
    repo_full_name: str,
    branch: str,
    token: str,
) -> None:
    clone_url = github_git_authenticated_url(repo_full_name, token)
    refspec = f"+refs/heads/{branch}:refs/remotes/dorf/{branch}"
    result = run_git_unchecked(
        repo,
        "fetch",
        "--no-tags",
        clone_url,
        refspec,
    )
    if result.returncode == 0:
        return
    message = result.stderr.strip() or result.stdout.strip() or "git fetch failed"
    message = redact_token(message, token)
    typer.echo(f"Could not fetch remote base branch objects: {message}", err=True)
    raise typer.Exit(result.returncode or 1)


def github_git_authenticated_url(repo_full_name: str, token: str) -> str:
    quoted_token = urllib.parse.quote(token, safe="")
    return f"https://x-access-token:{quoted_token}@github.com/{repo_full_name}.git"


def redact_token(message: str, token: str) -> str:
    encoded_token = urllib.parse.quote(token, safe="")
    return message.replace(token, "<redacted>").replace(encoded_token, "<redacted>")


def github_repo_full_name_or_exit(repo: Path) -> str:
    remote_url = run_git_unchecked(repo, "remote", "get-url", "origin")
    if remote_url.returncode != 0:
        typer.echo("Git remote not found: origin", err=True)
        raise typer.Exit(1)
    origin = remote_url.stdout.strip()
    parsed = parse_github_repo_full_name(origin)
    if parsed is None:
        typer.echo(f"Could not infer GitHub repo from origin: {origin}", err=True)
        raise typer.Exit(1)
    return parsed


def parse_github_repo_full_name(remote_url: str) -> str | None:
    https_match = re.match(
        r"^https://github\.com/([^/]+)/([^/]+?)(?:\.git)?/?$",
        remote_url,
    )
    if https_match is not None:
        return f"{https_match.group(1)}/{https_match.group(2)}"
    ssh_match = re.match(
        r"^(?:git@github\.com:|ssh://git@github\.com/)([^/]+)/([^/]+?)(?:\.git)?/?$",
        remote_url,
    )
    if ssh_match is not None:
        return f"{ssh_match.group(1)}/{ssh_match.group(2)}"
    return None


def is_dirty(repo: Path) -> bool:
    return bool(run_git(repo, "status", "--porcelain"))


def generate_job_name(task: str) -> str:
    return f"{secrets.token_hex(3)}-{slugify(task)}"


def slugify(value: str) -> str:
    slug = re.sub(r"[^a-z0-9]+", "-", value.lower()).strip("-")
    return slug[:40].strip("-") or "task"


def github_repository_client_from_app_token() -> GitHubRepositoryClient:
    config = load_github_app_config()
    minted = GitHubAppTokenClient().mint_installation_token(config)
    token = minted.token if isinstance(minted, GitHubInstallationToken) else str(minted)
    return GitHubRepositoryClient(token)


def load_contract_or_exit(repo: Path) -> RepoContract:
    try:
        return load_repo_contract(repo)
    except ContractValidationError as error:
        typer.echo(f"{CONTRACT_FILENAME}: {error}", err=True)
        raise typer.Exit(1) from error


def deployment_image_fingerprint(
    profile: DeploymentProfile | None,
    contract: RepoContract,
) -> str | None:
    """Use a setup fingerprint only when it describes the requested Room configuration."""
    explicit_template = contract.incus_config.get("template")
    if explicit_template is not None:
        if re.fullmatch(r"[0-9a-f]{64}", explicit_template):
            return explicit_template
        if profile is None or explicit_template != profile.incus.template:
            return None
    return profile.image_fingerprint if profile is not None else None


def select_coding_deployment_or_exit(
    override: str | None,
) -> tuple[str, DeploymentProfile | None]:
    """Select provider policy and immutable image identity for a new coding Room."""
    try:
        profile = load_optional_deployment_profile()
    except DeploymentProfileError as error:
        if override is not None:
            return override, None
        typer.echo(f"Could not load the global deployment profile: {error}", err=True)
        typer.echo("remediation: Run `dorf setup`.", err=True)
        raise typer.Exit(1) from error
    if override is not None:
        return override, profile
    if profile is None:
        typer.echo(
            "Dorf setup is incomplete: no default Provider Connection is configured.",
            err=True,
        )
        typer.echo("remediation: Run `dorf setup`.", err=True)
        raise typer.Exit(1)
    return profile.provider_connection, profile


def open_dorf(
    contract: RepoContract | None = None,
    *,
    deployment_profile: DeploymentProfile | None = None,
    provider_connection: str | None = None,
) -> Dorf:
    """Compose the in-process SDK with the same built-ins selected by the CLI."""
    return Dorf.open(
        environment_config=(
            deployment_profile.incus
            if deployment_profile is not None
            else IncusConfig.from_mapping(contract.incus_config if contract is not None else None)
        ),
        agent_defaults=contract.primary_codex if contract is not None else None,
        provider_connection=provider_connection,
    )


def _missing_setup_provider_connection(job_name: str) -> str:
    typer.echo(
        f"Coding Job {job_name} has no recorded Provider Connection; recreate it",
        err=True,
    )
    raise typer.Exit(1)


def echo_contract_summary(contract: RepoContract) -> None:
    typer.echo(f"Mode: {contract.mode}")
    if contract.commands:
        typer.echo(f"Commands: {', '.join(contract.commands)}")


def exit_if_environment_prerequisites_missing(
    dorf: Dorf,
) -> None:
    missing = dorf.environment_prerequisites()
    if not missing:
        return
    echo_environment_prerequisite_failures(missing)
    raise typer.Exit(1)


def echo_environment_prerequisite_failures(failures: list[str]) -> None:
    typer.echo("Incus VM fast preflight failed.", err=True)
    for item in failures:
        typer.echo(f"- {item}", err=True)
    typer.echo("Run `dorf doctor` for deep diagnosis.", err=True)


def run_git(cwd: Path, *args: str) -> str:
    result = run_git_unchecked(cwd, *args)
    if result.returncode != 0:
        message = result.stderr.strip() or result.stdout.strip() or "git command failed"
        typer.echo(message, err=True)
        raise typer.Exit(result.returncode)
    return result.stdout.strip()


def run_git_unchecked(cwd: Path, *args: str) -> subprocess.CompletedProcess[str]:
    return subprocess.run(
        ["git", *args],
        cwd=cwd,
        text=True,
        capture_output=True,
        check=False,
    )
