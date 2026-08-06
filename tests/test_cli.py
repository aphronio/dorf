import json
import socket
import subprocess
from dataclasses import replace
from pathlib import Path

import pytest
from click.exceptions import Exit
from typer.testing import CliRunner

from dorf import Dorf
from dorf.adapters.agents.codex_config import CodexConfig
from dorf.adapters.environments import (
    IncusCheckResult,
    IncusConfig,
    IncusFailure,
)
from dorf.cli import (
    CodingTask,
    GitBackedJobBranch,
    GitTarget,
    app,
    await_github_authority_approval,
    create_admitted_git_backed_job_branch_or_exit,
    create_git_backed_job_branch_or_exit,
    deployment_image_fingerprint,
    fetch_github_branch_objects_or_exit,
    github_issue_task,
    launch_coding_job_or_exit,
    prove_coding_admission_or_exit,
    recover_git_backed_job_branch_or_exit,
    resolve_git_author_or_exit,
    unsafe_dorf_branch_reason,
)
from dorf.coding_workspace import GitAuthorIdentity
from dorf.deployment_profile import (
    DeploymentProfile,
    load_deployment_profile,
    save_deployment_profile,
)
from dorf.github_app import (
    GitHubAppConfig,
    GitHubInstallationToken,
    GitHubIssue,
    GitHubRepositoryError,
)
from dorf.provider_gateway import (
    InferenceRoute,
    ProviderConnection,
    ProviderGateway,
)
from dorf.repo_contract import RepoContract
from dorf.runtime import ArtifactInput, RuntimeStore, WorkerWaitResult
from dorf.workflows import (
    AdmissionFailure,
    CodingAdmissionProof,
    CodingAdmissionRequest,
    CodingAdmissionResult,
    CodingStore,
    GitHubAuthorityApproval,
)


def configure_passing_incus(monkeypatch) -> list[list[str]]:
    monkeypatch.setattr(
        "dorf.adapters.environments.incus.IncusDoctor.fast_check",
        lambda self, config: type("Result", (), {"ok": True, "failures": []})(),
    )
    commands: list[list[str]] = []
    routes: dict[str, InferenceRoute] = {}

    def run(self, argv, *, input=None, timeout_seconds=None):
        commands.append(argv)
        if argv[:3] == ["incus", "network", "get"] and argv[-1] == "ipv4.address":
            return subprocess.CompletedProcess(argv, 0, "10.42.0.1/24\n", "")
        if "--" in argv:
            tail = argv[argv.index("--") + 1 :]
            if tail == ["ip", "-4", "route", "get", "1.1.1.1"]:
                return subprocess.CompletedProcess(
                    argv,
                    0,
                    "1.1.1.1 via 10.42.0.1 dev enp5s0 src 10.42.0.19 uid 0\n",
                    "",
                )
        return subprocess.CompletedProcess(argv, 0, "", "")

    monkeypatch.setattr("dorf.adapters.environments.incus.IncusRunnerProbe.run", run)
    monkeypatch.setattr(
        ProviderGateway,
        "require_connection",
        lambda self, connection_name: ProviderConnection(
            connection_name,
            "chatgpt",
            "subscription",
            "connected",
        ),
    )

    def create_route(self, connection_name, *, consumer, wire_api="responses"):
        route = InferenceRoute(
            f"route-{len(routes) + 1}",
            connection_name,
            "http://10.42.0.1:8317/v1",
            "responses",
            "room-key",
        )
        routes[consumer] = route
        return route

    monkeypatch.setattr(ProviderGateway, "create_route", create_route)
    monkeypatch.setattr(
        ProviderGateway,
        "route_for_consumer",
        lambda self, consumer: routes.get(consumer),
    )

    def revoke_route(self, route_id):
        consumer = next(
            (name for name, route in routes.items() if route.id == route_id),
            None,
        )
        if consumer is None:
            return False
        del routes[consumer]
        return True

    monkeypatch.setattr(ProviderGateway, "revoke_route", revoke_route)
    return commands


def write_cli_provider_broker(path: Path) -> None:
    path.write_text(
        """#!/usr/bin/env python3
import http.server
import json
import pathlib
import sys

if "-h" in sys.argv:
    print("Provider broker version: 7.2.104")
    raise SystemExit

config_path = pathlib.Path(sys.argv[sys.argv.index("-config") + 1])
config = config_path.read_text()
host = next(line.split(":", 1)[1].strip().strip('"') for line in config.splitlines()
            if line.startswith("host:"))
port = int(next(line.split(":", 1)[1].strip() for line in config.splitlines()
                if line.startswith("port:")))
auth_dir = pathlib.Path(next(line.split(":", 1)[1].strip().strip('"')
                             for line in config.splitlines()
                             if line.startswith("auth-dir:")))
if "-codex-device-login" in sys.argv:
    print("Codex device URL: https://auth.openai.com/codex/device")
    print("Codex device code: ABCD-EFGH")
    auth_dir.mkdir(parents=True, exist_ok=True)
    saved_path = auth_dir / "codex-private-account@example.com.json"
    saved_path.write_text('{"type":"codex","access_token":"upstream-secret"}')
    print(f"Authentication saved to {saved_path}")
    print("Codex device authentication successful!")
    raise SystemExit

class Handler(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        if self.path == "/v1/models":
            self.send_response(200)
            self.send_header("Content-Type", "application/json")
            self.end_headers()
            self.wfile.write(json.dumps({"data": [{"id": "gpt-5.6-sol"}]}).encode())
            return
        if self.path == "/v0/management/auth-files":
            files = [
                {
                    "name": credential.name,
                    "provider": "codex",
                    "runtime_only": False,
                    "status": "active",
                    "unavailable": False,
                    "id_token": {"plan_type": "pro"},
                }
                for credential in auth_dir.glob("codex-*.json")
            ]
            if "codex-api-key:" in config:
                files.append({
                    "name": "configured-openai-platform",
                    "provider": "codex",
                    "runtime_only": True,
                    "status": "active",
                    "unavailable": False,
                })
            self.send_response(200)
            self.send_header("Content-Type", "application/json")
            self.end_headers()
            self.wfile.write(json.dumps({"files": files}).encode())
            return
        self.send_response(200)
        self.end_headers()

    def do_PUT(self):
        self.send_response(200)
        self.end_headers()

    def log_message(self, format, *args):
        pass

http.server.ThreadingHTTPServer((host, port), Handler).serve_forever()
"""
    )
    path.chmod(0o700)


def free_provider_port() -> int:
    with socket.socket() as listener:
        listener.bind(("127.0.0.1", 0))
        return listener.getsockname()[1]


def create_git_repo(path: Path) -> Path:
    path.mkdir()
    git(path, "init", "-b", "main")
    git(path, "config", "user.name", "Dorf Tests")
    git(path, "config", "user.email", "dorf@example.com")
    (path / "README.md").write_text("demo\n")
    git(path, "add", "README.md")
    git(path, "commit", "-m", "initial")
    return path


def git(cwd: Path, *args: str) -> str:
    return subprocess.run(
        ["git", *args], cwd=cwd, text=True, capture_output=True, check=True
    ).stdout.strip()


def test_help_exposes_resource_first_l1_without_bare_compatibility_commands() -> None:
    result = CliRunner().invoke(app, ["--help"])

    assert result.exit_code == 0
    assert "worker" in result.output
    assert "job" in result.output
    for removed in ("spawn", "assign", "send", "inspect", "wait", "attach"):
        assert f"│ {removed}" not in result.output
    assert CliRunner().invoke(app, ["send", "anything", "hello"]).exit_code == 2


def test_doctor_reports_sanitized_provider_gateway_health(tmp_path, monkeypatch) -> None:
    monkeypatch.chdir(tmp_path)
    configure_passing_incus(monkeypatch)

    result = CliRunner().invoke(
        app,
        ["doctor"],
        env={
            "XDG_CONFIG_HOME": str(tmp_path / "config"),
            "XDG_DATA_HOME": str(tmp_path / "data"),
        },
    )

    assert result.exit_code == 0
    assert "provider-gateway: stopped" in result.output
    assert "provider-backend: missing (pinned 7.2.104)" in result.output
    assert "provider-bind: 127.0.0.1" in result.output
    assert "provider-connections: none" in result.output
    assert "CLIProxyAPI" not in result.output
    assert "deployment-profile: not configured; checking defaults" in result.output
    assert "incus-vm: ok" in result.output


def test_doctor_uses_the_global_core_profile_outside_repository_context(
    tmp_path,
    monkeypatch,
) -> None:
    commands = configure_passing_incus(monkeypatch)
    config_home = tmp_path / "config"
    save_deployment_profile(
        DeploymentProfile(
            provider_connection="personal-chatgpt",
            incus=IncusConfig(
                template="global-room-image",
                network="globalbr0",
                root_disk_size="40GiB",
            ),
        ),
        config_home=config_home,
    )
    monkeypatch.chdir(tmp_path)

    result = CliRunner().invoke(
        app,
        ["doctor"],
        env={
            "XDG_CONFIG_HOME": str(config_home),
            "XDG_DATA_HOME": str(tmp_path / "data"),
        },
    )

    assert result.exit_code == 0
    assert "deployment-profile: configured" in result.output
    assert [
        "incus",
        "launch",
        "global-room-image",
        next(command[3] for command in commands if command[:2] == ["incus", "launch"]),
        "--vm",
        "--network",
        "globalbr0",
    ] in commands


def test_doctor_failure_writes_the_same_simple_diagnostic_files(
    tmp_path,
    monkeypatch,
) -> None:
    configure_passing_incus(monkeypatch)
    monkeypatch.setattr(
        "dorf.cli.IncusDoctor.core_check",
        lambda self, config: IncusCheckResult(
            failures=[
                IncusFailure(
                    "guest-agent",
                    "Incus guest agent did not become ready",
                )
            ]
        ),
    )

    result = CliRunner().invoke(
        app,
        ["doctor"],
        env={
            "XDG_DATA_HOME": str(tmp_path / "data"),
            "XDG_STATE_HOME": str(tmp_path / "state"),
        },
    )

    assert result.exit_code == 1
    assert "Doctor failed" in result.output
    assert "Human-readable diagnostic:" in result.output
    assert "Agent-readable diagnostic:" in result.output
    bundles = list((tmp_path / "state" / "dorf" / "diagnostics").iterdir())
    payload = json.loads((bundles[0] / "diagnostic.json").read_text())
    assert payload["reproducer"] == ["dorf doctor"]
    assert payload["observed"] == ["guest-agent: Incus guest agent did not become ready"]


def test_provider_cli_and_direct_facade_share_one_named_api_key_connection(
    tmp_path,
    monkeypatch,
) -> None:
    state_path = tmp_path / "gateway"
    executable = tmp_path / "provider-broker"
    write_cli_provider_broker(executable)
    port = free_provider_port()
    open_gateway = ProviderGateway.open
    monkeypatch.setattr(
        "dorf.cli.ProviderGateway.open",
        classmethod(
            lambda cls: open_gateway(
                state_path=state_path,
                executable_path=executable,
                port=port,
            )
        ),
    )
    api_key = "platform-secret-that-must-not-appear"

    connected = CliRunner().invoke(
        app,
        [
            "provider",
            "connect",
            "openai",
            "--api-key",
            "--name",
            "work-openai",
        ],
        env={
            "OPENAI_API_KEY": api_key,
            "XDG_CONFIG_HOME": str(tmp_path / "config"),
        },
    )
    assert connected.exit_code == 0
    assert "work-openai · openai · api_key · connected" in connected.output
    assert api_key not in connected.output
    assert (
        load_deployment_profile(config_home=tmp_path / "config").provider_connection
        == "work-openai"
    )

    listed = CliRunner().invoke(app, ["provider", "list"])
    assert listed.exit_code == 0
    assert "work-openai · openai · api_key · connected" in listed.output

    inspected = CliRunner().invoke(
        app,
        ["provider", "status", "work-openai"],
    )
    assert inspected.exit_code == 0
    assert "work-openai · openai · api_key · connected" in inspected.output
    assert "CLIProxyAPI" not in inspected.output

    with open_gateway(
        state_path=state_path,
        executable_path=executable,
        port=port,
    ) as gateway:
        assert gateway.list_connections() == (
            ProviderConnection(
                name="work-openai",
                provider="openai",
                auth_mode="api_key",
                status="connected",
            ),
        )

    disconnected = CliRunner().invoke(
        app,
        ["provider", "disconnect", "work-openai"],
    )
    assert disconnected.exit_code == 0
    assert "Disconnected provider connection: work-openai" in disconnected.output
    assert CliRunner().invoke(app, ["provider", "list"]).output.strip() == (
        "No provider connections."
    )

    with open_gateway(
        state_path=state_path,
        executable_path=executable,
        port=port,
    ) as gateway:
        gateway.shutdown()


def test_provider_cli_completes_headless_chatgpt_device_connection_without_paths(
    tmp_path,
    monkeypatch,
) -> None:
    state_path = tmp_path / "gateway"
    executable = tmp_path / "provider-broker"
    write_cli_provider_broker(executable)
    port = free_provider_port()
    open_gateway = ProviderGateway.open
    monkeypatch.setattr(
        "dorf.cli.ProviderGateway.open",
        classmethod(
            lambda cls: open_gateway(
                state_path=state_path,
                executable_path=executable,
                port=port,
            )
        ),
    )

    connected = CliRunner().invoke(
        app,
        [
            "provider",
            "connect",
            "chatgpt",
            "--subscription",
            "--name",
            "personal-chatgpt",
        ],
        env={"XDG_CONFIG_HOME": str(tmp_path / "config")},
    )

    assert connected.exit_code == 0
    assert "Open: https://auth.openai.com/codex/device" in connected.output
    assert "Code: ABCD-EFGH" in connected.output
    assert "personal-chatgpt · chatgpt · subscription · connected" in connected.output
    assert "private-account@example.com" not in connected.output
    assert str(state_path) not in connected.output
    assert "CLIProxyAPI" not in connected.output

    status = CliRunner().invoke(
        app,
        ["provider", "status", "personal-chatgpt"],
    )
    assert status.exit_code == 0
    assert "personal-chatgpt · chatgpt · subscription · connected" in status.output
    assert "plan: pro" in status.output

    assert CliRunner().invoke(app, ["provider", "disconnect", "personal-chatgpt"]).exit_code == 0
    with open_gateway(
        state_path=state_path,
        executable_path=executable,
        port=port,
    ) as gateway:
        gateway.shutdown()


def test_worker_spawn_records_caller_policy_without_creating_job_documents(
    tmp_path, monkeypatch
) -> None:
    monkeypatch.chdir(tmp_path)
    commands = configure_passing_incus(monkeypatch)
    data_home = tmp_path / "data"

    result = CliRunner().invoke(
        app,
        [
            "worker",
            "spawn",
            "researcher",
            "--provider-connection",
            "personal-chatgpt",
        ],
        env={"XDG_DATA_HOME": str(data_home)},
    )

    assert result.exit_code == 0
    assert "researcher · ready" in result.output
    assert "provenance: caller" in result.output
    assert "lifecycle policy: caller-managed" in result.output
    assert "current Job: none" in result.output
    assert not (data_home / "dorf" / "jobs" / "researcher").exists()
    room_commands = [
        command for command in commands if command[:2] in (["incus", "init"], ["incus", "start"])
    ]
    assert room_commands[:2] == [
        [
            "incus",
            "init",
            "dorf-codex",
            "dorf-researcher",
            "--vm",
            "--network",
            "incusbr0",
            "-d",
            "root,size=40GiB",
        ],
        ["incus", "start", "dorf-researcher"],
    ]


def test_worker_spawn_uses_global_profile_without_reading_repo_contract(
    tmp_path,
    monkeypatch,
) -> None:
    repo = create_git_repo(tmp_path / "repo")
    (repo / ".dorf.toml").write_text("this is deliberately invalid")
    monkeypatch.chdir(repo)
    commands = configure_passing_incus(monkeypatch)
    monkeypatch.setattr(
        "dorf.sdk.launch_worker_message_dispatcher",
        lambda *args: False,
    )
    monkeypatch.setattr(
        "dorf.sdk.launch_job_input_dispatcher",
        lambda *args: False,
    )
    monkeypatch.setattr(
        "dorf.sdk.launch_assignment_report_collector",
        lambda *args: True,
    )
    config_home = tmp_path / "config"
    data_home = tmp_path / "data"
    DeploymentProfile(
        provider_connection="personal-chatgpt",
        incus=IncusConfig(
            template="dorf-codex-validated",
            network="incusbr0",
            root_disk_size="40GiB",
        ),
    ).save(config_home=config_home)

    result = CliRunner().invoke(
        app,
        ["worker", "spawn", "researcher"],
        env={
            "XDG_CONFIG_HOME": str(config_home),
            "XDG_DATA_HOME": str(data_home),
        },
    )

    assert result.exit_code == 0
    assert "researcher · ready" in result.output
    direct_message = CliRunner().invoke(
        app,
        ["worker", "message", "researcher", "Say hello"],
        env={"XDG_DATA_HOME": str(data_home)},
    )
    assignment = CliRunner().invoke(
        app,
        [
            "job",
            "assign",
            "research",
            "--to",
            "researcher",
            "--goal",
            "Investigate the question",
        ],
        env={"XDG_DATA_HOME": str(data_home)},
    )
    assert direct_message.exit_code == 0
    assert assignment.exit_code == 0
    assert "research · open" in assignment.output
    assert [
        "incus",
        "init",
        "dorf-codex-validated",
        "dorf-researcher",
        "--vm",
        "--network",
        "incusbr0",
        "-d",
        "root,size=40GiB",
    ] in commands


def test_worker_spawn_without_profile_explains_the_setup_boundary(
    tmp_path,
    monkeypatch,
) -> None:
    monkeypatch.chdir(tmp_path)

    result = CliRunner().invoke(
        app,
        ["worker", "spawn", "researcher"],
        env={
            "XDG_CONFIG_HOME": str(tmp_path / "config"),
            "XDG_DATA_HOME": str(tmp_path / "data"),
        },
    )

    assert result.exit_code == 1
    assert "setup is incomplete" in result.output
    assert "dorf provider connect --help" in result.output
    assert "--provider-connection" not in result.output


def test_worker_recover_reports_room_loss_without_replacement_and_allows_ending(
    tmp_path, monkeypatch
) -> None:
    monkeypatch.chdir(tmp_path)
    commands = configure_passing_incus(monkeypatch)
    data_home = tmp_path / "data"
    env = {"XDG_DATA_HOME": str(data_home)}
    runner = CliRunner()
    monkeypatch.setattr("dorf.sdk.launch_job_input_dispatcher", lambda *args: False)
    monkeypatch.setattr("dorf.sdk.launch_assignment_report_collector", lambda *args: True)
    assert (
        runner.invoke(
            app,
            [
                "worker",
                "spawn",
                "researcher",
                "--provider-connection",
                "personal-chatgpt",
            ],
            env=env,
        ).exit_code
        == 0
    )
    assert (
        runner.invoke(
            app,
            [
                "job",
                "assign",
                "checkout-perf",
                "--to",
                "researcher",
                "--goal",
                "Make checkout instant",
            ],
            env=env,
        ).exit_code
        == 0
    )

    def absent(self, argv, *, input=None, timeout_seconds=None):
        commands.append(argv)
        if argv[:3] == ["incus", "network", "get"] and argv[-1] == "ipv4.address":
            return subprocess.CompletedProcess(argv, 0, "10.42.0.1/24\n", "")
        if tuple(argv[:3]) in {
            ("incus", "info", "dorf-researcher"),
            ("incus", "delete", "dorf-researcher"),
        }:
            return subprocess.CompletedProcess(argv, 1, "", "Instance not found")
        return subprocess.CompletedProcess(argv, 0, "", "")

    monkeypatch.setattr("dorf.adapters.environments.incus.IncusRunnerProbe.run", absent)
    recovered = runner.invoke(app, ["worker", "recover", "researcher"], env=env)
    store = RuntimeStore.open(data_home / "dorf" / "state.sqlite3")
    job_before_end = store.get_job_binding("checkout-perf")

    assert recovered.exit_code == 1
    assert "automatic replacement is unsupported" in recovered.output
    assert store.get_worker("researcher").status == "offline"
    assert store.get_worker("researcher").current_room_id is None
    assert store.get_room(job_before_end.room.id).status == "absent"
    assert job_before_end.assignment.generation == 1
    assert len([command for command in commands if command[:2] == ["incus", "init"]]) == 1
    worker_pulse = runner.invoke(app, ["worker", "inspect", "researcher"], env=env)
    job_pulse = runner.invoke(app, ["job", "inspect", "checkout-perf"], env=env)
    assert worker_pulse.exit_code == job_pulse.exit_code == 0
    assert "researcher · offline" in worker_pulse.output
    assert "room: absent" in worker_pulse.output
    assert "checkout-perf · open" in job_pulse.output
    assert "assigned: researcher (generation 1)" in job_pulse.output
    assert "room: unavailable (recorded absent)" in job_pulse.output

    ended_job = runner.invoke(app, ["job", "end", "checkout-perf", "--interrupt"], env=env)
    ended_worker = runner.invoke(app, ["worker", "end", "researcher"], env=env)

    assert ended_job.exit_code == 0, ended_job.output
    assert (
        "Room already absent; local workspace and processes were already gone" in ended_job.output
    )
    assert ended_worker.exit_code == 0, ended_worker.output
    assert store.get_job("checkout-perf").status == "ended"
    assert store.get_worker("researcher").status == "ended"


def test_worker_attach_uses_current_room_and_always_starts_at_workspace(
    tmp_path, monkeypatch
) -> None:
    monkeypatch.chdir(tmp_path)
    configure_passing_incus(monkeypatch)
    attached = []

    def attach(self, argv):
        attached.append(argv)
        return subprocess.CompletedProcess(argv, 0, "", "")

    monkeypatch.setattr("dorf.adapters.environments.incus.IncusRunnerProbe.attach", attach)
    monkeypatch.setattr("dorf.sdk.launch_job_input_dispatcher", lambda *args: False)
    monkeypatch.setattr("dorf.sdk.launch_assignment_report_collector", lambda *args: True)
    data_home = tmp_path / "data"
    env = {"XDG_DATA_HOME": str(data_home)}
    runner = CliRunner()
    assert (
        runner.invoke(
            app,
            [
                "worker",
                "spawn",
                "researcher",
                "--provider-connection",
                "personal-chatgpt",
            ],
            env=env,
        ).exit_code
        == 0
    )
    assert (
        runner.invoke(
            app,
            [
                "job",
                "assign",
                "checkout-perf",
                "--to",
                "researcher",
                "--goal",
                "Make checkout instant",
            ],
            env=env,
        ).exit_code
        == 0
    )

    result = runner.invoke(app, ["worker", "attach", "researcher"], env=env)
    inspected = runner.invoke(app, ["worker", "inspect", "researcher"], env=env)

    assert result.exit_code == 0, result.output
    assert "Entering researcher Room at /workspace" in result.output
    assert "Attachment ended" in result.output
    assert attached == [
        [
            "incus",
            "exec",
            "dorf-researcher",
            "--cwd",
            "/workspace",
            "--mode",
            "interactive",
            "--",
            "bash",
        ]
    ]
    assert "human presence: detached" in inspected.output
    assert (
        CodingStore.open(data_home / "dorf" / "state.sqlite3").get_worker_presence("researcher")
        is None
    )


def test_worker_attach_propagates_shell_exit_and_still_clears_presence(
    tmp_path, monkeypatch
) -> None:
    monkeypatch.chdir(tmp_path)
    configure_passing_incus(monkeypatch)
    monkeypatch.setattr(
        "dorf.adapters.environments.incus.IncusRunnerProbe.attach",
        lambda self, argv: subprocess.CompletedProcess(argv, 7, None, None),
    )
    data_home = tmp_path / "data"
    env = {"XDG_DATA_HOME": str(data_home)}
    runner = CliRunner()
    assert (
        runner.invoke(
            app,
            [
                "worker",
                "spawn",
                "researcher",
                "--provider-connection",
                "personal-chatgpt",
            ],
            env=env,
        ).exit_code
        == 0
    )

    result = runner.invoke(app, ["worker", "attach", "researcher"], env=env)

    assert result.exit_code == 7
    assert "Attachment ended: researcher" in result.output
    assert (
        CodingStore.open(data_home / "dorf" / "state.sqlite3").get_worker_presence("researcher")
        is None
    )


def test_worker_message_retains_offline_input_and_wait_pins_exact_message(
    tmp_path, monkeypatch
) -> None:
    monkeypatch.chdir(tmp_path)
    configure_passing_incus(monkeypatch)
    data_home = tmp_path / "data"
    env = {"XDG_DATA_HOME": str(data_home)}
    assert (
        CliRunner()
        .invoke(
            app,
            [
                "worker",
                "spawn",
                "researcher",
                "--provider-connection",
                "personal-chatgpt",
            ],
            env=env,
        )
        .exit_code
        == 0
    )
    monkeypatch.setattr("dorf.sdk.launch_worker_message_dispatcher", lambda *args: False)

    first = CliRunner().invoke(
        app,
        [
            "worker",
            "message",
            "researcher",
            "hello",
            "--model",
            "gpt-5.7",
            "--reasoning-effort",
            "xhigh",
            "--json",
        ],
        env=env,
    )
    receipt = json.loads(first.output)
    store = RuntimeStore.open(data_home / "dorf" / "state.sqlite3")
    binding = store.get_worker_binding("researcher")
    assert binding is not None
    store.update_room_status(binding.room.id, "failed", "Room offline")
    second = CliRunner().invoke(
        app, ["worker", "message", "researcher", "when you return", "--json"], env=env
    )

    assert first.exit_code == second.exit_code == 0
    assert json.loads(second.output)["delivery"] == "pending"
    assert [item.text for item in store.list_worker_messages("researcher")] == [
        "hello",
        "when you return",
    ]

    observed = []

    def observe(self, worker_name, message_id):
        observed.append(message_id)
        return WorkerWaitResult(
            "pending-approval",
            "2026-07-27T12:00:00+00:00",
            message_id,
            1,
            response="I need approval.",
            detail="Approve publication",
        )

    monkeypatch.setattr("dorf.sdk.WorkerRuntime.observe_wait", observe)
    waited = CliRunner().invoke(
        app,
        ["worker", "wait", "researcher", "--message", receipt["message_id"]],
        env=env,
    )
    assert waited.exit_code == 0
    assert observed == [receipt["message_id"]]
    assert "Response:\nI need approval." in waited.output
    assert "Need: Approve publication" in waited.output


def test_resource_end_commands_require_intent_and_leave_no_room_ghost(
    tmp_path, monkeypatch
) -> None:
    monkeypatch.chdir(tmp_path)
    commands = configure_passing_incus(monkeypatch)
    data_home = tmp_path / "data"
    env = {"XDG_DATA_HOME": str(data_home)}
    runner = CliRunner()
    monkeypatch.setattr("dorf.sdk.launch_job_input_dispatcher", lambda *args: False)
    monkeypatch.setattr("dorf.sdk.launch_assignment_report_collector", lambda *args: True)
    assert (
        runner.invoke(
            app,
            [
                "worker",
                "spawn",
                "researcher",
                "--provider-connection",
                "personal-chatgpt",
            ],
            env=env,
        ).exit_code
        == 0
    )
    assert (
        runner.invoke(
            app,
            [
                "job",
                "assign",
                "checkout-perf",
                "--to",
                "researcher",
                "--goal",
                "Make checkout instant",
            ],
            env=env,
        ).exit_code
        == 0
    )

    refused = runner.invoke(app, ["job", "end", "checkout-perf"], env=env)
    ended_job = runner.invoke(app, ["job", "end", "checkout-perf", "--interrupt"], env=env)
    ended_worker = runner.invoke(app, ["worker", "end", "researcher"], env=env)

    assert refused.exit_code == 1
    assert "job wait checkout-perf" in refused.output
    assert ended_job.exit_code == 0, ended_job.output
    assert "Released Worker: researcher" in ended_job.output
    assert ended_worker.exit_code == 0, ended_worker.output
    assert "Room destroyed: dorf-researcher" in ended_worker.output
    store = RuntimeStore.open(data_home / "dorf" / "state.sqlite3")
    assert store.get_job("checkout-perf").status == "ended"
    assert store.get_assignment("checkout-perf").status == "ended"
    assert store.get_worker("researcher").status == "ended"
    assert store.get_current_room("researcher") is None
    assert [command[:3] for command in commands if command[:2] == ["incus", "delete"]] == [
        ["incus", "delete", "dorf-researcher"]
    ]


def test_job_resource_cli_uses_goal_backed_assignment_and_independent_fifo(
    tmp_path, monkeypatch
) -> None:
    monkeypatch.chdir(tmp_path)
    configure_passing_incus(monkeypatch)
    data_home = tmp_path / "data"
    env = {"XDG_DATA_HOME": str(data_home)}
    assert (
        CliRunner()
        .invoke(
            app,
            [
                "worker",
                "spawn",
                "researcher",
                "--provider-connection",
                "personal-chatgpt",
            ],
            env=env,
        )
        .exit_code
        == 0
    )
    monkeypatch.setattr("dorf.sdk.launch_job_input_dispatcher", lambda *args: False)
    monkeypatch.setattr("dorf.sdk.launch_assignment_report_collector", lambda *args: True)

    assigned = CliRunner().invoke(
        app,
        [
            "job",
            "assign",
            "checkout-perf",
            "--to",
            "researcher",
            "--goal",
            "Make checkout instant",
        ],
        env=env,
    )
    messaged = CliRunner().invoke(
        app, ["job", "message", "checkout-perf", "Profile the API first"], env=env
    )
    inspected = CliRunner().invoke(app, ["job", "inspect", "checkout-perf"], env=env)

    assert assigned.exit_code == messaged.exit_code == inspected.exit_code == 0
    assert "goal v1: Make checkout instant" in assigned.output
    assert "workspace: /workspace/jobs/checkout-perf" in assigned.output
    assert "Queued message 2 for Job checkout-perf" in messaged.output
    assert "queued inputs: 2" in inspected.output
    assert "assigned: researcher (generation 1)" in inspected.output


def test_job_document_lenses_survive_unavailable_room(tmp_path, monkeypatch) -> None:
    monkeypatch.chdir(tmp_path)
    configure_passing_incus(monkeypatch)
    data_home = tmp_path / "data"
    env = {"XDG_DATA_HOME": str(data_home)}
    monkeypatch.setattr("dorf.sdk.launch_job_input_dispatcher", lambda *args: False)
    monkeypatch.setattr("dorf.sdk.launch_assignment_report_collector", lambda *args: True)
    assert (
        CliRunner()
        .invoke(
            app,
            [
                "worker",
                "spawn",
                "researcher",
                "--provider-connection",
                "personal-chatgpt",
            ],
            env=env,
        )
        .exit_code
        == 0
    )
    assert (
        CliRunner()
        .invoke(
            app,
            [
                "job",
                "assign",
                "checkout-perf",
                "--to",
                "researcher",
                "--goal",
                "Make checkout instant",
            ],
            env=env,
        )
        .exit_code
        == 0
    )
    store = RuntimeStore.open(data_home / "dorf" / "state.sqlite3")
    binding = store.get_job_binding("checkout-perf")
    assert binding is not None
    artifact = tmp_path / "profile.txt"
    artifact.write_text("p95=120ms\n")
    store.documents.append_event(
        "checkout-perf",
        event_id="report-profile",
        source="worker",
        provenance="claim",
        kind="evidence",
        summary="Captured checkout profile",
        related={"assignment": binding.assignment.id},
        artifacts=[ArtifactInput("profile.txt", artifact, "text/plain")],
    )
    store.update_room_status(binding.room.id, "failed", "Room stopped")

    timeline = CliRunner().invoke(app, ["job", "inspect", "checkout-perf", "--timeline"], env=env)
    evidence = CliRunner().invoke(app, ["job", "inspect", "checkout-perf", "--evidence"], env=env)
    pulse = CliRunner().invoke(app, ["job", "inspect", "checkout-perf"], env=env)

    assert timeline.exit_code == evidence.exit_code == pulse.exit_code == 0
    assert "[worker claim] Captured checkout profile" in timeline.output
    assert "profile.txt · text/plain · 10 bytes" in evidence.output
    assert "room: unavailable (recorded failed): Room stopped" in pulse.output


def test_job_artifact_cli_lists_and_exports_binary_after_room_cleanup(
    tmp_path,
    monkeypatch,
) -> None:
    monkeypatch.chdir(tmp_path)
    configure_passing_incus(monkeypatch)
    data_home = tmp_path / "data"
    env = {"XDG_DATA_HOME": str(data_home)}
    monkeypatch.setattr("dorf.sdk.launch_job_input_dispatcher", lambda *args: False)
    monkeypatch.setattr("dorf.sdk.launch_assignment_report_collector", lambda *args: True)
    runner = CliRunner()
    assert (
        runner.invoke(
            app,
            [
                "worker",
                "spawn",
                "researcher",
                "--provider-connection",
                "personal-chatgpt",
            ],
            env=env,
        ).exit_code
        == 0
    )
    assert (
        runner.invoke(
            app,
            [
                "job",
                "assign",
                "checkout-perf",
                "--to",
                "researcher",
                "--goal",
                "Retain the binary profile",
            ],
            env=env,
        ).exit_code
        == 0
    )
    database = data_home / "dorf" / "state.sqlite3"
    store = RuntimeStore.open(database)
    binding = store.get_job_binding("checkout-perf")
    assert binding is not None
    content = bytes(range(256)) * 300
    source = tmp_path / "profile.bin"
    source.write_bytes(content)
    store.documents.append_event(
        "checkout-perf",
        event_id="report-profile",
        source="worker",
        provenance="claim",
        kind="completion",
        summary="Reported the binary profile",
        related={"assignment": binding.assignment.id},
        artifacts=[ArtifactInput("profile.bin", source, "application/octet-stream")],
    )
    store.close()
    assert (
        runner.invoke(
            app,
            ["job", "end", "checkout-perf", "--interrupt"],
            env=env,
        ).exit_code
        == 0
    )
    assert runner.invoke(app, ["worker", "end", "researcher"], env=env).exit_code == 0

    with Dorf.open(database) as dorf:
        artifact = dorf.list_job_artifacts("checkout-perf")[0]
    export_directory = tmp_path / "exports"
    export_directory.mkdir()
    listed = runner.invoke(
        app,
        ["job", "artifact", "list", "checkout-perf"],
        env=env,
    )
    exported = runner.invoke(
        app,
        [
            "job",
            "artifact",
            "export",
            "checkout-perf",
            artifact.ref,
            "--to",
            str(export_directory),
        ],
        env=env,
    )
    destination = export_directory / "profile.bin"
    destination.write_bytes(b"keep local bytes")
    collision = runner.invoke(
        app,
        [
            "job",
            "artifact",
            "export",
            "checkout-perf",
            artifact.ref,
            "--to",
            str(export_directory),
        ],
        env=env,
    )
    kept_bytes = destination.read_bytes()
    overwritten = runner.invoke(
        app,
        [
            "job",
            "artifact",
            "export",
            "checkout-perf",
            artifact.ref,
            "--to",
            str(export_directory),
            "--overwrite",
        ],
        env=env,
    )
    cross_job = runner.invoke(
        app,
        [
            "job",
            "artifact",
            "export",
            "checkout-perf",
            f"artifact-v1-{artifact.job_id + 1}-{'0' * 64}",
            "--to",
            str(export_directory),
        ],
        env=env,
    )

    assert listed.exit_code == 0, listed.output
    assert artifact.ref in listed.output
    assert "profile.bin · application/octet-stream · 76800 bytes" in listed.output
    assert (
        "sha256:f8b0585eb91f58c007a5634362c9f90d8543822c113f702523bc7b73408a9392" in listed.output
    )
    assert "worker claim" in listed.output
    assert "artifacts/sha256" not in listed.output
    assert exported.exit_code == 0, exported.output
    assert f"Exported profile.bin to {export_directory / 'profile.bin'}" in exported.output
    assert "application/octet-stream · 76800 bytes" in exported.output
    assert collision.exit_code == 1
    assert "Destination already exists" in collision.output
    assert "Use --overwrite" in collision.output
    assert kept_bytes == b"keep local bytes"
    assert overwritten.exit_code == 0, overwritten.output
    assert destination.read_bytes() == content
    assert cross_job.exit_code == 1
    assert "belongs to another Job" in cross_job.output


def test_coding_start_composes_dedicated_worker_job_and_independent_clone(
    tmp_path, monkeypatch
) -> None:
    repo = create_git_repo(tmp_path / "repo")
    (repo / ".dorf.toml").write_text(
        '[commands]\nprepare = "uv sync --frozen"\ncheck = "true"\n'
    )
    git(repo, "add", ".dorf.toml")
    git(repo, "commit", "-m", "contract")
    monkeypatch.chdir(repo)
    commands = configure_passing_incus(monkeypatch)
    monkeypatch.setattr("dorf.cli.generate_job_name", lambda task: "abc123-demo-task")

    def create_branch(target, branch_name, before_create=None):
        branch = GitBackedJobBranch(
            "example/repo",
            target.start_sha,
            {
                "github_repo": "example/repo",
                "github_remote_branch_status": "pending",
            },
            "installation-token",
        )
        if before_create is not None:
            before_create(branch)
        return branch

    monkeypatch.setattr("dorf.cli.create_git_backed_job_branch_or_exit", create_branch)
    setup_order = []

    def observe_preparation(store, environment, binding, contract):
        setup_order.append(("prepare", binding.assignment.status))

    monkeypatch.setattr(
        "dorf.cli.run_repository_preparation_or_raise",
        observe_preparation,
    )
    dispatched = []

    def observe_dispatch(database, name):
        assignment = CodingStore.open(database).get_job_binding(name).assignment
        setup_order.append(("dispatch", assignment.status))
        dispatched.append((database, name))
        return False

    monkeypatch.setattr(
        "dorf.sdk.launch_job_input_dispatcher",
        observe_dispatch,
    )
    monkeypatch.setattr("dorf.sdk.launch_assignment_report_collector", lambda *args: True)
    data_home = tmp_path / "data"
    config_home = tmp_path / "config"
    image_fingerprint = "f" * 64
    DeploymentProfile(
        provider_connection="personal-chatgpt",
        image_fingerprint=image_fingerprint,
    ).save(config_home=config_home)
    env = {
        "XDG_CONFIG_HOME": str(config_home),
        "XDG_DATA_HOME": str(data_home),
    }

    result = CliRunner().invoke(
        app,
        [
            "start",
            "Demo task",
        ],
        env=env,
    )
    worker = CliRunner().invoke(app, ["worker", "inspect", "coder-abc123-demo-task"], env=env)
    job = CliRunner().invoke(app, ["job", "inspect", "abc123-demo-task"], env=env)
    job_json = CliRunner().invoke(app, ["job", "inspect", "abc123-demo-task", "--json"], env=env)
    coding = CliRunner().invoke(app, ["status", "abc123-demo-task"], env=env)
    admitted = CliRunner().invoke(app, ["acceptance", "abc123-demo-task", "--json"], env=env)
    dossier = CliRunner().invoke(app, ["dossier", "abc123-demo-task", "--json"], env=env)

    assert (
        result.exit_code
        == worker.exit_code
        == job.exit_code
        == job_json.exit_code
        == admitted.exit_code
        == dossier.exit_code
        == 0
    )
    assert coding.exit_code == 0
    assert "Worker: coder-abc123-demo-task (coding-workflow, dedicated)" in result.output
    assert "Workspace: /workspace/jobs/abc123-demo-task" in result.output
    assert "Acceptance: draft (2 items" in result.output
    assert "provenance: coding-workflow" in worker.output
    assert "lifecycle policy: dedicated" in worker.output
    assert "goal v1: Demo task" in job.output
    assert "Working rules:" not in job.output
    pulse = json.loads(job_json.output)
    checklist = json.loads(admitted.output)
    proof = json.loads(dossier.output)
    assert checklist["state"] == "draft"
    assert [item["key"] for item in checklist["items"]] == ["goal-1", "repo-check"]
    assert checklist["items"][0]["verifier"] == "command"
    assert proof["commit_sha"] == git(repo, "rev-parse", "HEAD")
    assert proof["acceptance"][0]["status"] == "unproven"
    assert proof["environment"]["environment_type"] == "incus-vm"
    assert ["image_fingerprint", image_fingerprint] in proof["environment"]["metadata"]
    assert not any(
        "immutable image fingerprint" in risk for risk in proof["unresolved_risks"]
    )
    assert pulse["job"] == "abc123-demo-task"
    assert pulse["goal_summary"] == "Demo task"
    assert "Working rules:" in pulse["goal"]
    assert pulse["outcome_stage"] == "active"
    assert pulse["lifecycle"]["state"] == "open"
    assert pulse["lifecycle"]["source"] == "runtime"
    assert pulse["lifecycle"]["provenance"] == "fact"
    assert pulse["attention"]["state"] == "quiet"
    assert pulse["evidence_count"] == 0
    assert pulse["room_availability"]["status"] == "available"
    assert pulse["room_availability"]["source"] == "runtime"
    assert pulse["room_availability"]["provenance"] == "fact"
    assert "lifecycle [runtime fact]: open" in job.output
    assert "Room availability [runtime fact]: available" in job.output
    assert {"room", "workspace", "conversation"}.isdisjoint(pulse)
    assert "Worker provenance: coding-workflow" in coding.output
    clone_commands = [command for command in commands if "git clone" in " ".join(command)]
    assert len(clone_commands) == 1
    clone = " ".join(clone_commands[0])
    assert "/workspace/jobs/abc123-demo-task" in clone
    assert "git worktree" not in clone
    assert "rm -rf" not in clone
    store = CodingStore.open(data_home / "dorf" / "state.sqlite3")
    assert store.get_job("abc123-demo-task").status == "open"
    coding_job = store.get_coding_job("abc123-demo-task")
    assert "setup_model" not in coding_job.metadata
    assert "setup_reasoning_effort" not in coding_job.metadata
    assert "setup_task_prompt" not in coding_job.metadata
    assert coding_job.metadata["github_remote_branch_status"] == "created"
    assert store.get_job_binding("abc123-demo-task").room.metadata["provider_connection"] == (
        "personal-chatgpt"
    )
    assert (
        store.get_job_binding("abc123-demo-task").room.metadata["image_fingerprint"]
        == image_fingerprint
    )
    assert any(
        command[:3] == ["incus", "init", image_fingerprint] for command in commands
    )
    binding = store.get_job_binding("abc123-demo-task")
    store.update_room_status(
        binding.room.id,
        "failed",
        f"Incus VM {binding.room.provider_id} stopped",
    )
    unavailable_text = CliRunner().invoke(
        app, ["job", "inspect", "abc123-demo-task"], env=env
    )
    unavailable_json = CliRunner().invoke(
        app, ["job", "inspect", "abc123-demo-task", "--json"], env=env
    )
    unavailable = json.loads(unavailable_json.output)
    assert unavailable_text.exit_code == unavailable_json.exit_code == 0
    assert "Room availability [runtime fact]: unavailable (Incus VM Room stopped)" in (
        unavailable_text.output
    )
    assert unavailable["room_availability"]["status"] == "unavailable"
    assert unavailable["room_availability"]["detail"] == "Incus VM Room stopped"
    assert binding.room.id not in unavailable_json.output
    assert binding.room.provider_id not in unavailable_json.output
    assert len(dispatched) == 1
    assert setup_order == [("prepare", "preparing"), ("dispatch", "open")]
    assert dispatched[0][1] == "abc123-demo-task"
    assert store.list_job_inputs("abc123-demo-task")[0].kind == "goal"


def test_repository_incus_override_does_not_claim_global_image_fingerprint() -> None:
    profile = DeploymentProfile(
        provider_connection="personal-chatgpt",
        image_fingerprint="f" * 64,
    )
    contract = RepoContract(
        mode="configured",
        commands={},
        env={},
        incus_config={"template": "repo-specific-image"},
    )

    assert deployment_image_fingerprint(profile, contract) is None


@pytest.mark.parametrize(
    "incus_override",
    [
        {"network": "repo-network"},
        {"root_disk_size": "64GiB"},
        {"network": "repo-network", "root_disk_size": "64GiB"},
    ],
)
def test_repository_non_image_incus_overrides_preserve_global_image_fingerprint(
    incus_override,
) -> None:
    fingerprint = "f" * 64
    profile = DeploymentProfile(
        provider_connection="personal-chatgpt",
        image_fingerprint=fingerprint,
    )
    contract = RepoContract(
        mode="configured",
        commands={},
        env={},
        incus_config=incus_override,
    )

    assert deployment_image_fingerprint(profile, contract) == fingerprint


def test_explicit_fingerprint_template_becomes_the_immutable_image_pin() -> None:
    profile_fingerprint = "f" * 64
    explicit_fingerprint = "e" * 64
    profile = DeploymentProfile(
        provider_connection="personal-chatgpt",
        image_fingerprint=profile_fingerprint,
    )
    contract = RepoContract(
        mode="configured",
        commands={},
        env={},
        incus_config={"template": explicit_fingerprint},
    )

    assert deployment_image_fingerprint(profile, contract) == explicit_fingerprint
    assert deployment_image_fingerprint(None, contract) == explicit_fingerprint


def test_new_coding_job_without_provider_default_fails_before_branch_or_job_mutation(
    tmp_path, monkeypatch
) -> None:
    repo = create_git_repo(tmp_path / "repo")
    monkeypatch.chdir(repo)
    monkeypatch.setattr("dorf.cli.generate_job_name", lambda task: "no-provider")
    branch_attempts = []
    monkeypatch.setattr(
        "dorf.cli.create_git_backed_job_branch_or_exit",
        lambda *args, **kwargs: branch_attempts.append((args, kwargs)),
    )
    data_home = tmp_path / "data"

    result = CliRunner().invoke(
        app,
        ["start", "No provider"],
        env={
            "XDG_CONFIG_HOME": str(tmp_path / "config"),
            "XDG_DATA_HOME": str(data_home),
        },
    )

    assert result.exit_code == 1
    assert "no default Provider Connection" in result.output
    assert "dorf setup" in result.output
    assert branch_attempts == []
    assert CodingStore.open(data_home / "dorf" / "state.sqlite3").get_coding_job(
        "no-provider"
    ) is None


def test_afk_aggregated_admission_failure_precedes_coordinator_and_job_mutation(
    tmp_path, monkeypatch
) -> None:
    repo = create_git_repo(tmp_path / "repo")
    monkeypatch.chdir(repo)
    coordinator_attempts = []
    branch_attempts = []
    monkeypatch.setattr(
        "dorf.cli.CodingWorkflow.prepare_afk_start",
        lambda *args, **kwargs: coordinator_attempts.append((args, kwargs)),
    )
    monkeypatch.setattr(
        "dorf.cli.create_admitted_git_backed_job_branch_or_exit",
        lambda *args, **kwargs: branch_attempts.append((args, kwargs)),
    )
    failures = (
        AdmissionFailure(
            "github-authority",
            "GitHub App owner",
            "Issue access failed.",
            "Run `dorf github setup`.",
            "The exact issue is unproved.",
            False,
        ),
        AdmissionFailure(
            "provider-route",
            "provider connection owner",
            "Provider route failed.",
            "Reconnect the provider.",
            "Implementation cannot execute.",
            False,
        ),
    )
    monkeypatch.setattr(
        "dorf.cli.CodingAdmissionPreflight.prove",
        lambda self, request: CodingAdmissionResult(failures=failures),
    )
    data_home = tmp_path / "data"

    result = CliRunner().invoke(
        app,
        ["afk", "18"],
        env={"XDG_DATA_HOME": str(data_home)},
    )

    assert result.exit_code == 1
    assert "Coding admission failed with 2 independently discovered failure(s)" in result.output
    assert "owner: GitHub App owner" in result.output
    assert "repair: Run `dorf github setup`." in result.output
    assert "automatic continuation: no" in result.output
    assert coordinator_attempts == branch_attempts == []
    store = CodingStore.open(data_home / "dorf" / "state.sqlite3")
    assert store.list_coding_jobs() == []
    assert store.get_afk_coordinator(str(repo.resolve()), 18) is None


def test_start_rejects_task_and_issue_before_coding_admission(tmp_path, monkeypatch) -> None:
    repo = create_git_repo(tmp_path / "repo")
    monkeypatch.chdir(repo)
    admission_attempts = []
    monkeypatch.setattr(
        "dorf.cli.prove_coding_admission_or_exit",
        lambda *args, **kwargs: admission_attempts.append((args, kwargs)),
    )

    result = CliRunner().invoke(app, ["start", "Conflicting task", "--issue", "18"])

    assert result.exit_code == 1
    assert "Provide exactly one of TASK or --issue." in result.output
    assert admission_attempts == []


def test_afk_success_admits_original_issue_with_the_exact_recorded_proof(
    tmp_path, monkeypatch
) -> None:
    repo = create_git_repo(tmp_path / "repo")
    monkeypatch.chdir(repo)
    incus_commands = configure_passing_incus(monkeypatch)
    contract = RepoContract(
        mode="configured", commands={"check": "uv run pytest"}, env={}
    )
    admission = CodingAdmissionProof.create(
        repository="example/repo",
        installation_id="123",
        issue=GitHubIssue(18, "One admission proof", "Issue body", ()),
        target_branch="main",
        target_start_sha=git(repo, "rev-parse", "HEAD"),
        image_fingerprint="b" * 64,
        provider_connection="personal-chatgpt",
        reviewer="codex",
        contract=contract,
        codex_config=CodexConfig("gpt-5.6-sol", "low"),
        git_author=GitAuthorIdentity("Dorf Tests", "dorf@example.com"),
        environment_config=IncusConfig(
            template="b" * 64,
            network="admission-net",
            root_disk_size="73GiB",
        ),
        installation_token="installation-token",
    )
    preflight_requests = []
    reservation_snapshots = []

    def prove(self, request):
        preflight_requests.append(request)
        return CodingAdmissionResult(proof=admission)

    monkeypatch.setattr("dorf.cli.CodingAdmissionPreflight.prove", prove)
    monkeypatch.setattr("dorf.cli.generate_job_name", lambda task: "admitted-issue")

    def create_branch(target, branch_name, proof, before_create=None):
        remote = GitBackedJobBranch(
            proof.repository,
            proof.target_start_sha,
            {"github_repo": proof.repository, "github_remote_branch_status": "pending"},
            proof.installation_token,
        )
        if before_create is not None:
            before_create(remote)
            reserved_store = CodingStore.open()
            reserved_job = reserved_store.get_coding_job("admitted-issue")
            reserved_checklist = reserved_store.get_acceptance_checklist(
                "admitted-issue"
            )
            reservation_snapshots.append(
                (
                    reserved_job.status,
                    reserved_checklist.state,
                    tuple(item.key for item in reserved_checklist.items),
                )
            )
        return remote

    monkeypatch.setattr(
        "dorf.cli.create_admitted_git_backed_job_branch_or_exit",
        create_branch,
    )
    monkeypatch.setattr("dorf.sdk.launch_job_input_dispatcher", lambda *args: False)
    monkeypatch.setattr("dorf.sdk.launch_assignment_report_collector", lambda *args: True)
    coordinated = []
    monkeypatch.setattr(
        "dorf.cli.run_coding_job_workflow_or_exit",
        lambda job_name, action: coordinated.append(job_name),
    )
    data_home = tmp_path / "data"

    result = CliRunner().invoke(
        app,
        ["afk", "18"],
        env={"XDG_DATA_HOME": str(data_home)},
    )

    assert result.exit_code == 0, result.output
    assert f"Coding admission ready: {admission.proof_id}" in result.output
    assert "Started coding Job admitted-issue" in result.output
    assert coordinated == ["admitted-issue"]
    assert reservation_snapshots == [
        ("setting-up", "draft", ("goal-1", "repo-check"))
    ]
    store = CodingStore.open(data_home / "dorf" / "state.sqlite3")
    job = store.get_coding_job("admitted-issue")
    assert json.loads(job.metadata["admission_proof"]) == admission.record()
    checklist = store.get_acceptance_checklist("admitted-issue")
    assert checklist.state == "draft"
    assert [item.verifier_command for item in checklist.items] == [
        "uv run pytest",
        "uv run pytest",
    ]
    goal = store.get_job("admitted-issue").goal
    assert goal.startswith("Implement this coding task as Dorf Job admitted-issue.")
    assert "Task:\n        Issue #18: One admission proof" in goal
    timeline = store.documents.list_events("admitted-issue")
    assert any(
        event.summary == f"Coding admission proved by {admission.proof_id}"
        for event in timeline
    )
    binding = store.get_job_binding("admitted-issue")
    assert binding.metadata["provider_connection"] == admission.provider_connection
    assert binding.metadata["template"] == admission.environment_config.template
    assert binding.metadata["network"] == admission.environment_config.network
    assert (
        binding.metadata["root_disk_size"]
        == admission.environment_config.root_disk_size
    )
    assert binding.metadata["image_fingerprint"] == admission.image_fingerprint
    assert any(
        command[:3] == ["incus", "init", admission.image_fingerprint]
        and "admission-net" in command
        and "root,size=73GiB" in command
        for command in incus_commands
    )
    coordinator = store.get_afk_coordinator(str(repo.resolve()), 18)
    store.finish_afk_coordinator(
        str(repo.resolve()),
        18,
        coordinator.owner_token,
        "ready",
    )

    repeated = CliRunner().invoke(
        app,
        ["afk", "18"],
        env={"XDG_DATA_HOME": str(data_home)},
    )

    assert repeated.exit_code == 0, repeated.output
    assert f"Reusing coding admission proof: {admission.proof_id}" in repeated.output
    assert len(preflight_requests) == 1
    assert len(store.list_coding_jobs()) == 1
    assert coordinated == ["admitted-issue", "admitted-issue"]


def test_afk_missing_github_authority_resumes_original_delegation_after_approval(
    tmp_path, monkeypatch
) -> None:
    repo = create_git_repo(tmp_path / "repo")
    monkeypatch.chdir(repo)
    configure_passing_incus(monkeypatch)
    approval = GitHubAuthorityApproval(
        installation_id="123",
        missing_authority="Dorf GitHub App access to example/repo",
        why_needed="Read issue #20 and publish its branch and PR.",
        action="Add example/repo to installation 123 and save access.",
        scope="Only example/repo for this delegation.",
        approve_consequence="Dorf may read the issue and publish the coding proposal.",
        decline_consequence="The delegation ends without resources.",
        automatic_resume="Exact readiness reruns and this delegation continues.",
        url="https://github.com/settings/installations/123",
        repository="example/repo",
    )
    missing = AdmissionFailure(
        "github-repository-authority",
        "GitHub App installation owner",
        "The installation cannot access example/repo.",
        approval.action,
        approval.approve_consequence,
        True,
        approval,
    )
    admission = CodingAdmissionProof.create(
        repository="example/repo",
        installation_id="123",
        issue=GitHubIssue(20, "Resume one authority approval", "Issue body", ()),
        target_branch="main",
        target_start_sha=git(repo, "rev-parse", "HEAD"),
        image_fingerprint="b" * 64,
        provider_connection="personal-chatgpt",
        reviewer="codex",
        contract=RepoContract(mode="configured", commands={}, env={}),
        codex_config=CodexConfig("gpt-5.6-sol", "low"),
        git_author=GitAuthorIdentity("Dorf Tests", "dorf@example.com"),
        environment_config=IncusConfig(template="b" * 64),
        installation_token="installation-token",
    )
    proofs = iter(
        [CodingAdmissionResult(failures=(missing,)), CodingAdmissionResult(proof=admission)]
    )
    preflight_requests = []

    def prove(self, request):
        preflight_requests.append(request)
        return next(proofs)

    monkeypatch.setattr("dorf.cli.CodingAdmissionPreflight.prove", prove)
    monkeypatch.setattr(
        "dorf.cli.await_github_authority_approval", lambda attempt: "approved"
    )
    monkeypatch.setattr("dorf.cli.generate_job_name", lambda task: "approved-issue")
    monkeypatch.setattr(
        "dorf.cli.create_admitted_git_backed_job_branch_or_exit",
        lambda target, branch_name, proof, before_create=None: (
            before_create(
                GitBackedJobBranch(
                    proof.repository,
                    proof.target_start_sha,
                    {"github_repo": proof.repository},
                    proof.installation_token,
                )
            )
            or GitBackedJobBranch(
                proof.repository,
                proof.target_start_sha,
                {"github_repo": proof.repository},
                proof.installation_token,
            )
        ),
    )
    monkeypatch.setattr("dorf.sdk.launch_job_input_dispatcher", lambda *args: False)
    monkeypatch.setattr("dorf.sdk.launch_assignment_report_collector", lambda *args: True)
    monkeypatch.setattr("dorf.cli.run_coding_job_workflow_or_exit", lambda *args: None)
    data_home = tmp_path / "data"

    result = CliRunner().invoke(
        app, ["afk", "20"], env={"XDG_DATA_HOME": str(data_home)}
    )

    assert result.exit_code == 0, result.output
    assert "Attention: GitHub authority approval required" in result.output
    assert "Missing authority: Dorf GitHub App access to example/repo" in result.output
    assert "Automatic resume: Exact readiness reruns" in result.output
    assert "GitHub authority approved; rerunning exact coding readiness." in result.output
    assert len(preflight_requests) == 2
    assert preflight_requests[0].repository is None
    assert preflight_requests[1] == replace(
        preflight_requests[0], repository="example/repo", installation_id="123"
    )
    store = CodingStore.open(data_home / "dorf" / "state.sqlite3")
    attempts = store.list_coding_admissions()
    assert len(attempts) == 1
    assert attempts[0].status == "admitted"
    assert attempts[0].job_name == "approved-issue"
    assert attempts[0].request["command"] == "afk"
    assert attempts[0].request["target_start_sha"] == git(repo, "rev-parse", "HEAD")
    assert len(store.list_coding_jobs()) == 1


@pytest.mark.parametrize(
    ("attempt_status", "ttl_seconds", "reused"),
    [("pending", 3600, True), ("approved", 3600, True), ("pending", -1, False)],
)
def test_successful_preflight_reuses_active_attempt_after_controller_restart(
    tmp_path, monkeypatch, attempt_status, ttl_seconds, reused
) -> None:
    repo = create_git_repo(tmp_path / "repo")
    start_sha = git(repo, "rev-parse", "HEAD")
    target = GitTarget(repo, "main", start_sha)
    request = CodingAdmissionRequest(
        repo_path=str(repo),
        target_branch="main",
        issue_number=20,
        command="afk",
        target_start_sha=start_sha,
        repository="example/repo",
        installation_id="123",
    )
    approval = GitHubAuthorityApproval(
        installation_id="123",
        missing_authority="Dorf GitHub App access to example/repo",
        why_needed="Read issue #20 and publish the proposal.",
        action="Add example/repo to installation 123.",
        scope="Only example/repo.",
        approve_consequence="The delegation can continue.",
        decline_consequence="No resources are created.",
        automatic_resume="Exact readiness reruns automatically.",
        url="https://github.com/settings/installations/123",
        repository="example/repo",
    )
    data_home = tmp_path / "data"
    monkeypatch.setenv("XDG_DATA_HOME", str(data_home))
    store = CodingStore.open(data_home / "dorf" / "state.sqlite3")
    attempt, _ = store.retain_pending_coding_admission(
        request.record(), approval.record(), ttl_seconds=ttl_seconds
    )
    if attempt_status == "approved":
        store.approve_coding_admission(attempt.id)
    admission = CodingAdmissionProof.create(
        repository="example/repo",
        installation_id="123",
        issue=GitHubIssue(20, "Resume one authority approval", "Issue body", ()),
        target_branch="main",
        target_start_sha=start_sha,
        image_fingerprint="b" * 64,
        provider_connection="personal-chatgpt",
        reviewer="codex",
        contract=RepoContract(mode="configured", commands={}, env={}),
        codex_config=CodexConfig("gpt-5.6-sol", "low"),
        git_author=GitAuthorIdentity("Dorf Tests", "dorf@example.com"),
        environment_config=IncusConfig(template="b" * 64),
        installation_token="installation-token",
    )
    monkeypatch.setattr(
        "dorf.cli.CodingAdmissionPreflight.prove",
        lambda self, request: CodingAdmissionResult(proof=admission),
    )

    resumed = prove_coding_admission_or_exit(
        target,
        command="afk",
        issue_number=20,
        model=None,
        reasoning_effort=None,
        provider_connection=None,
    )

    assert resumed.approval_attempt_id == (attempt.id if reused else None)
    assert store.get_coding_admission(attempt.id).status == (
        "approved" if reused else "expired"
    )


def test_authority_polling_rejects_a_configured_installation_swap(
    tmp_path, monkeypatch
) -> None:
    request = CodingAdmissionRequest(
        repo_path=str(tmp_path / "repo"),
        target_branch="main",
        issue_number=20,
        repository="example/repo",
        installation_id="123",
    )
    approval = GitHubAuthorityApproval(
        installation_id="123",
        missing_authority="Persistent access for installation 123",
        why_needed="Read the issue and publish the proposal.",
        action="Select example/repo for installation 123.",
        scope="Persistent repository-wide authority.",
        approve_consequence="Installation 123 gains repository access.",
        decline_consequence="No repository access is added.",
        automatic_resume="Only installation 123 is polled.",
        url="https://github.com/settings/installations/123",
        repository="example/repo",
    )
    store = CodingStore.open(tmp_path / "state.sqlite3")
    attempt, _ = store.retain_pending_coding_admission(
        request.record(), approval.record(), ttl_seconds=3600
    )
    monkeypatch.setattr("dorf.cli.webbrowser.open", lambda url: True)
    monkeypatch.setattr(
        "dorf.cli.load_github_app_config",
        lambda: GitHubAppConfig(app_id="app", installation_id="456"),
    )
    minted = []
    monkeypatch.setattr(
        "dorf.cli.GitHubAppTokenClient.mint_installation_token",
        lambda self, config: minted.append(config),
    )

    outcome = await_github_authority_approval(attempt)

    assert outcome == "installation-changed"
    assert minted == []
    assert store.get_coding_admission(attempt.id).status == "pending"


def test_post_reservation_failure_distinguishes_owner_from_concurrent_loser(
    tmp_path, monkeypatch
) -> None:
    repo = create_git_repo(tmp_path / "repo")
    configure_passing_incus(monkeypatch)
    start_sha = git(repo, "rev-parse", "HEAD")
    target = GitTarget(repo, "main", start_sha)
    data_home = tmp_path / "data"
    monkeypatch.setenv("XDG_DATA_HOME", str(data_home))
    request = CodingAdmissionRequest(
        repo_path=str(repo),
        target_branch="main",
        issue_number=20,
        command="afk",
        target_start_sha=start_sha,
        repository="example/repo",
        installation_id="123",
    )
    approval = GitHubAuthorityApproval(
        installation_id="123",
        missing_authority="Dorf GitHub App access to example/repo",
        why_needed="Read issue #20 and publish the proposal.",
        action="Add example/repo to installation 123.",
        scope="Only example/repo.",
        approve_consequence="The delegation can continue.",
        decline_consequence="No resources are created.",
        automatic_resume="Exact readiness reruns automatically.",
        url="https://github.com/settings/installations/123",
        repository="example/repo",
    )
    store = CodingStore.open(data_home / "dorf" / "state.sqlite3")
    attempt, _ = store.retain_pending_coding_admission(
        request.record(), approval.record(), ttl_seconds=3600
    )
    store.approve_coding_admission(attempt.id)
    admission = replace(
        CodingAdmissionProof.create(
            repository="example/repo",
            installation_id="123",
            issue=GitHubIssue(20, "Branch failure", "Issue body", ()),
            target_branch="main",
            target_start_sha=start_sha,
            image_fingerprint="b" * 64,
            provider_connection="personal-chatgpt",
            reviewer="codex",
            contract=RepoContract(mode="configured", commands={}, env={}),
            codex_config=CodexConfig("gpt-5.6-sol", "low"),
            git_author=GitAuthorIdentity("Dorf Tests", "dorf@example.com"),
            environment_config=IncusConfig(template="b" * 64),
            installation_token="installation-token",
        ),
        approval_attempt_id=attempt.id,
    )
    monkeypatch.setattr("dorf.cli.generate_job_name", lambda task: "branch-failure")

    def fail_after_reservation(target, branch_name, proof, before_create=None):
        assert before_create is not None
        before_create(
            GitBackedJobBranch(
                proof.repository,
                proof.target_start_sha,
                {"github_repo": proof.repository},
                proof.installation_token,
            )
        )
        raise RuntimeError("malformed GitHub response")

    monkeypatch.setattr(
        "dorf.cli.create_admitted_git_backed_job_branch_or_exit",
        fail_after_reservation,
    )

    with pytest.raises(Exit) as raised:
        launch_coding_job_or_exit(
            target,
            CodingTask("Issue #20: Branch failure", "Issue body"),
            provider_connection=None,
            admission_proof=admission,
        )

    assert raised.value.exit_code == 1
    assert store.get_coding_job("branch-failure").status == "setup-failed"
    retained = store.get_coding_admission(attempt.id)
    assert retained.status == "admitted"
    assert retained.job_name == "branch-failure"

    monkeypatch.setattr("dorf.cli.generate_job_name", lambda task: "concurrent-loser")
    with pytest.raises(Exit) as loser:
        launch_coding_job_or_exit(
            target,
            CodingTask("Issue #20: Branch failure", "Issue body"),
            provider_connection=None,
            admission_proof=admission,
        )

    assert loser.value.exit_code == 0
    assert store.get_coding_job("concurrent-loser") is None


@pytest.mark.parametrize(
    ("ready_repository", "ready_installation_id"),
    [("other/repo", "123"), ("example/repo", "456")],
)
def test_successful_preflight_rejects_retained_authority_identity_change(
    tmp_path, monkeypatch, ready_repository, ready_installation_id
) -> None:
    repo = create_git_repo(tmp_path / "repo")
    start_sha = git(repo, "rev-parse", "HEAD")
    target = GitTarget(repo, "main", start_sha)
    original = CodingAdmissionRequest(
        repo_path=str(repo),
        target_branch="main",
        issue_number=20,
        command="afk",
        target_start_sha=start_sha,
        repository="example/repo",
        installation_id="123",
    )
    approval = GitHubAuthorityApproval(
        installation_id="123",
        missing_authority="Dorf GitHub App access to example/repo",
        why_needed="Read issue #20 and publish the proposal.",
        action="Add example/repo to installation 123.",
        scope="Only example/repo.",
        approve_consequence="The delegation can continue.",
        decline_consequence="No resources are created.",
        automatic_resume="Exact readiness reruns automatically.",
        url="https://github.com/settings/installations/123",
        repository="example/repo",
    )
    data_home = tmp_path / "data"
    monkeypatch.setenv("XDG_DATA_HOME", str(data_home))
    store = CodingStore.open(data_home / "dorf" / "state.sqlite3")
    attempt, _ = store.retain_pending_coding_admission(
        original.record(), approval.record(), ttl_seconds=3600
    )
    changed = CodingAdmissionProof.create(
        repository=ready_repository,
        installation_id=ready_installation_id,
        issue=GitHubIssue(20, "Changed origin", "Issue body", ()),
        target_branch="main",
        target_start_sha=start_sha,
        image_fingerprint="b" * 64,
        provider_connection="personal-chatgpt",
        reviewer="codex",
        contract=RepoContract(mode="configured", commands={}, env={}),
        codex_config=CodexConfig("gpt-5.6-sol", "low"),
        git_author=GitAuthorIdentity("Dorf Tests", "dorf@example.com"),
        environment_config=IncusConfig(template="b" * 64),
        installation_token="installation-token",
    )
    monkeypatch.setattr(
        "dorf.cli.CodingAdmissionPreflight.prove",
        lambda self, request: CodingAdmissionResult(proof=changed),
    )

    with pytest.raises(Exit):
        prove_coding_admission_or_exit(
            target,
            command="afk",
            issue_number=20,
            model=None,
            reasoning_effort=None,
            provider_connection=None,
        )

    assert store.get_coding_admission(attempt.id).status == "pending"
    assert store.list_coding_jobs() == []


@pytest.mark.parametrize("outcome", ["declined", "expired"])
def test_afk_github_authority_decline_or_expiry_creates_no_active_state(
    tmp_path, monkeypatch, outcome
) -> None:
    repo = create_git_repo(tmp_path / "repo")
    monkeypatch.chdir(repo)
    approval = GitHubAuthorityApproval(
        installation_id="123",
        missing_authority="Dorf GitHub App access to example/repo",
        why_needed="Read issue #20 and publish the proposal.",
        action="Add example/repo to installation 123.",
        scope="Only example/repo.",
        approve_consequence="The delegation can continue.",
        decline_consequence="No resources are created.",
        automatic_resume="Exact readiness reruns automatically.",
        url="https://github.com/settings/installations/123",
        repository="example/repo",
    )
    failure = AdmissionFailure(
        "github-repository-authority",
        "GitHub App installation owner",
        "The installation cannot access example/repo.",
        approval.action,
        approval.approve_consequence,
        True,
        approval,
    )
    monkeypatch.setattr(
        "dorf.cli.CodingAdmissionPreflight.prove",
        lambda self, request: CodingAdmissionResult(failures=(failure,)),
    )
    monkeypatch.setattr(
        "dorf.cli.await_github_authority_approval", lambda attempt: outcome
    )
    data_home = tmp_path / "data"

    result = CliRunner().invoke(
        app, ["afk", "20"], env={"XDG_DATA_HOME": str(data_home)}
    )

    assert result.exit_code == 1
    assert f"GitHub authority approval {outcome}" in result.output
    store = CodingStore.open(data_home / "dorf" / "state.sqlite3")
    assert store.list_coding_admissions()[0].status == outcome
    assert store.list_coding_jobs() == []
    assert store.get_afk_coordinator(str(repo.resolve()), 20) is None


def test_coding_start_retries_setup_on_the_same_worker_job_and_assignment(
    tmp_path, monkeypatch
) -> None:
    repo = create_git_repo(tmp_path / "repo")
    monkeypatch.chdir(repo)
    configure_passing_incus(monkeypatch)
    monkeypatch.setattr("dorf.cli.generate_job_name", lambda task: "retry-task")
    branch = GitBackedJobBranch(
        "example/repo",
        git(repo, "rev-parse", "HEAD"),
        {"github_repo": "example/repo", "github_remote_branch_status": "pending"},
        "installation-token",
    )
    created = []
    recovered = []

    def create_branch(target, branch_name, before_create=None):
        created.append((target, branch_name))
        if before_create is not None:
            before_create(branch)
        return branch

    monkeypatch.setattr("dorf.cli.create_git_backed_job_branch_or_exit", create_branch)
    monkeypatch.setattr(
        "dorf.cli.recover_git_backed_job_branch_or_exit",
        lambda job: recovered.append(job.job_name) or branch,
    )
    monkeypatch.setattr("dorf.sdk.launch_job_input_dispatcher", lambda *args: False)
    monkeypatch.setattr("dorf.sdk.launch_assignment_report_collector", lambda *args: True)
    clone_attempts = 0

    def run(self, argv, *, input=None, timeout_seconds=None):
        nonlocal clone_attempts
        if argv[:3] == ["incus", "network", "get"] and argv[-1] == "ipv4.address":
            return subprocess.CompletedProcess(argv, 0, "10.42.0.1/24\n", "")
        if "git clone" in " ".join(argv):
            clone_attempts += 1
            if clone_attempts == 1:
                return subprocess.CompletedProcess(argv, 1, "", "clone failed")
        if argv[-5:] == ["ip", "-4", "route", "get", "1.1.1.1"]:
            return subprocess.CompletedProcess(
                argv,
                0,
                "1.1.1.1 via 10.42.0.1 dev enp5s0 src 10.42.0.19 uid 0\n",
                "",
            )
        return subprocess.CompletedProcess(argv, 0, "", "")

    monkeypatch.setattr("dorf.adapters.environments.incus.IncusRunnerProbe.run", run)
    data_home = tmp_path / "data"
    config_home = tmp_path / "config"
    DeploymentProfile(provider_connection="work-openai").save(config_home=config_home)
    env = {
        "XDG_CONFIG_HOME": str(config_home),
        "XDG_DATA_HOME": str(data_home),
    }
    runner = CliRunner()

    first = runner.invoke(
        app,
        [
            "start",
            "Retry task",
            "--provider-connection",
            "personal-chatgpt",
        ],
        env=env,
    )
    store = CodingStore.open(data_home / "dorf" / "state.sqlite3")
    initial = store.get_job_binding("retry-task")
    assert first.exit_code == 1
    failed_job = store.get_coding_job("retry-task")
    assert failed_job is not None, first.output
    assert failed_job.status == "setup-failed"
    admitted = store.get_acceptance_checklist("retry-task")
    assert admitted is not None
    assert admitted.items == ()
    assert "--resume retry-task" in first.output
    assert initial is not None
    assert initial.room.metadata["provider_connection"] == "personal-chatgpt"
    store.update_assignment_status("retry-task", "workspace-failed")

    DeploymentProfile(provider_connection="changed-default").save(config_home=config_home)
    (repo / ".dorf.toml").write_text('[commands]\ncheck = "new-command"\n')
    git(repo, "add", ".dorf.toml")
    git(repo, "commit", "-m", "change contract after admission")

    second = runner.invoke(
        app,
        [
            "start",
            "Retry task",
            "--resume",
            "retry-task",
        ],
        env={**env, "XDG_CONFIG_HOME": str(config_home)},
    )
    final = store.get_job_binding("retry-task")

    assert second.exit_code == 0, second.output
    assert store.get_coding_job("retry-task").status == "active"
    assert final.assignment.status == "open"
    assert final.worker.id == initial.worker.id
    assert final.room.id == initial.room.id
    assert final.conversation.id == initial.conversation.id
    assert store.get_worker("coder-retry-task").id == initial.worker.id
    assert store.get_job("retry-task").id == initial.job.id
    assert final.assignment.id == initial.assignment.id
    assert final.room.metadata["provider_connection"] == "personal-chatgpt"
    assert store.get_acceptance_checklist("retry-task") == admitted
    assert created and recovered == ["retry-task"]
    assert clone_attempts == 2


def test_coding_setup_resume_reconciles_a_persisted_reservation_before_resources(
    tmp_path, monkeypatch
) -> None:
    repo = create_git_repo(tmp_path / "repo")
    monkeypatch.chdir(repo)
    configure_passing_incus(monkeypatch)
    data_home = tmp_path / "data"
    database = data_home / "dorf" / "state.sqlite3"
    store = CodingStore.open(database)
    base_sha = git(repo, "rev-parse", "HEAD")
    store.create_coding_job(
        job_name="reserved-task",
        status="setting-up",
        metadata={
            "task": "Reserved task",
            "target_repo": str(repo),
            "target_branch": "main",
            "target_start_sha": base_sha,
            "job_branch": "dorf/reserved-task",
            "github_repo": "example/repo",
            "setup_model": "gpt-5.6-sol",
            "setup_reasoning_effort": "high",
            "setup_task_prompt": "Reserved task",
            "setup_provider_connection": "personal-chatgpt",
        },
    )
    monkeypatch.setattr(
        "dorf.cli.recover_git_backed_job_branch_or_exit",
        lambda job: GitBackedJobBranch("example/repo", base_sha, {}, "installation-token"),
    )
    monkeypatch.setattr("dorf.sdk.launch_job_input_dispatcher", lambda *args: False)
    monkeypatch.setattr("dorf.sdk.launch_assignment_report_collector", lambda *args: True)

    result = CliRunner().invoke(
        app,
        [
            "start",
            "Reserved task",
            "--resume",
            "reserved-task",
            "--provider-connection",
            "personal-chatgpt",
        ],
        env={"XDG_DATA_HOME": str(data_home)},
    )

    assert result.exit_code == 0, result.output
    job = store.get_coding_job("reserved-task")
    binding = store.get_job_binding("reserved-task")
    assert job.status == "active"
    assert binding.worker.name == "coder-reserved-task"
    assert binding.assignment.status == "open"
    assert binding.job.goal.startswith("Implement this coding task as Dorf Job reserved-task")
    assert "setup_model" not in job.metadata
    assert "setup_reasoning_effort" not in job.metadata
    assert "setup_task_prompt" not in job.metadata


def test_coding_reservation_is_durable_before_remote_branch_creation(tmp_path, monkeypatch) -> None:
    repo = create_git_repo(tmp_path / "repo")
    base_sha = git(repo, "rev-parse", "HEAD")
    target = type("Target", (), {"repo": repo, "branch": "main"})()
    events = []
    monkeypatch.setattr("dorf.cli.github_repo_full_name_or_exit", lambda path: "example/repo")
    monkeypatch.setattr(
        "dorf.cli.load_github_app_config",
        lambda: GitHubAppConfig(app_id="123", installation_id="456"),
    )
    monkeypatch.setattr(
        "dorf.cli.GitHubAppTokenClient.mint_installation_token",
        lambda self, config: GitHubInstallationToken(token="installation-token"),
    )

    class GitHub:
        def __init__(self, token):
            pass

        def get_branch_sha(self, repo, branch):
            return base_sha

        def create_branch(self, repo, branch, sha):
            events.append(("branch", branch, sha))

    monkeypatch.setattr("dorf.cli.GitHubRepositoryClient", GitHub)

    create_git_backed_job_branch_or_exit(
        target,
        "dorf/demo-task",
        before_create=lambda branch: events.append(
            (
                "reservation",
                branch.base_sha,
                branch.metadata["github_remote_branch_status"],
            )
        ),
    )

    assert events == [
        ("reservation", base_sha, "pending"),
        ("branch", "dorf/demo-task", base_sha),
    ]


@pytest.mark.parametrize("command_name", ["check", "smoke"])
def test_standalone_verification_command_freezes_acceptance_before_running(
    tmp_path, monkeypatch, command_name
) -> None:
    data_home = tmp_path / "data"
    monkeypatch.setenv("XDG_DATA_HOME", str(data_home))
    repo = tmp_path / "repo"
    repo.mkdir()
    (repo / ".dorf.toml").write_text(
        '[commands]\ncheck = "pytest"\nsmoke = "./smoke.sh"\n'
    )
    store = CodingStore.open()
    store.create_coding_job(
        job_name="proof",
        status="active",
        metadata={"target_repo": str(repo)},
    )
    store.record_acceptance_checklist(
        "proof",
        goal="Pinned goal",
        items=(),
    )
    observed_states = []

    def run_command(**kwargs):
        observed_states.append(store.get_acceptance_checklist("proof").state)
        return type("Run", (), {"exit_code": 0})()

    monkeypatch.setattr(
        "dorf.cli.get_runnable_coding_job_or_exit",
        lambda actual_store, job_name: actual_store.get_coding_job(job_name),
    )
    monkeypatch.setattr("dorf.cli.run_environment_job_command", run_command)

    result = CliRunner().invoke(app, [command_name, "proof"])

    assert result.exit_code == 0, result.output
    assert observed_states == ["governing"]
    assert store.get_acceptance_checklist("proof").state == "governing"


def test_admitted_branch_creation_rejects_target_advance_before_reservation(
    tmp_path, monkeypatch
) -> None:
    base_sha = "b" * 40
    advanced_sha = "c" * 40
    target = type(
        "Target",
        (),
        {"repo": tmp_path / "repo", "branch": "main", "start_sha": base_sha},
    )()
    admission = CodingAdmissionProof.create(
        repository="example/repo",
        installation_id="123",
        issue=GitHubIssue(18, "One admission proof", "Issue body", ()),
        target_branch="main",
        target_start_sha=base_sha,
        image_fingerprint="d" * 64,
        provider_connection="personal-chatgpt",
        reviewer="codex",
        contract=RepoContract(mode="configured", commands={}, env={}),
        codex_config=CodexConfig("gpt-5.6-sol", "low"),
        git_author=GitAuthorIdentity("Dorf Tests", "dorf@example.com"),
        environment_config=IncusConfig(template="d" * 64),
        installation_token="installation-token",
    )
    events = []

    class GitHub:
        def __init__(self, token):
            assert token == "installation-token"

        def get_branch_sha(self, repo, branch):
            return advanced_sha

        def create_branch(self, repo, branch, sha):
            events.append(("branch", branch, sha))

    monkeypatch.setattr("dorf.cli.GitHubRepositoryClient", GitHub)

    with pytest.raises(Exit):
        create_admitted_git_backed_job_branch_or_exit(
            target,
            "dorf/demo-task",
            admission,
            before_create=lambda branch: events.append(("reservation", branch.base_sha)),
        )

    assert events == []


@pytest.mark.parametrize("branch_state", ["present", "missing"])
def test_setup_recovery_reuses_or_recreates_only_the_recorded_job_branch(
    tmp_path, monkeypatch, branch_state
) -> None:
    base_sha = "b" * 40
    store = CodingStore.open(tmp_path / "state.sqlite3")
    job = store.create_coding_job(
        job_name="demo-task",
        status="setup-failed",
        metadata={
            "github_repo": "example/repo",
            "target_branch": "main",
            "target_start_sha": base_sha,
            "job_branch": "dorf/demo-task",
        },
    )
    monkeypatch.setattr(
        "dorf.cli.load_github_app_config",
        lambda: GitHubAppConfig(app_id="123", installation_id="456"),
    )
    monkeypatch.setattr(
        "dorf.cli.GitHubAppTokenClient.mint_installation_token",
        lambda self, config: GitHubInstallationToken(token="installation-token"),
    )
    created = []

    class GitHub:
        def __init__(self, token):
            assert token == "installation-token"

        def get_branch_sha(self, repo, branch):
            if branch_state == "missing":
                raise GitHubRepositoryError("GitHub API HTTP 404: Not Found")
            return base_sha

        def create_branch(self, repo, branch, sha):
            created.append((repo, branch, sha))

    monkeypatch.setattr("dorf.cli.GitHubRepositoryClient", GitHub)

    recovered = recover_git_backed_job_branch_or_exit(job)

    assert (recovered.repo_full_name, recovered.base_sha, recovered.token) == (
        "example/repo",
        base_sha,
        "installation-token",
    )
    assert created == (
        [("example/repo", "dorf/demo-task", base_sha)] if branch_state == "missing" else []
    )


def test_setup_recovery_rejects_moved_branch_and_missing_repository_metadata(
    tmp_path, monkeypatch
) -> None:
    store = CodingStore.open(tmp_path / "state.sqlite3")
    metadata = {
        "github_repo": "example/repo",
        "target_branch": "main",
        "target_start_sha": "b" * 40,
        "job_branch": "dorf/demo-task",
    }
    job = store.create_coding_job(job_name="demo-task", status="setup-failed", metadata=metadata)
    missing = store.create_coding_job(
        job_name="missing-repo",
        status="setup-failed",
        metadata={
            "target_branch": "main",
            "target_start_sha": "b" * 40,
            "job_branch": "dorf/missing-repo",
        },
    )
    monkeypatch.setattr(
        "dorf.cli.load_github_app_config",
        lambda: GitHubAppConfig(app_id="123", installation_id="456"),
    )
    monkeypatch.setattr(
        "dorf.cli.GitHubAppTokenClient.mint_installation_token",
        lambda self, config: GitHubInstallationToken(token="installation-token"),
    )

    class GitHub:
        def __init__(self, token):
            pass

        def get_branch_sha(self, repo, branch):
            return "c" * 40

    monkeypatch.setattr("dorf.cli.GitHubRepositoryClient", GitHub)

    with pytest.raises(Exit):
        recover_git_backed_job_branch_or_exit(job)
    with pytest.raises(Exit):
        recover_git_backed_job_branch_or_exit(missing)


def test_authenticated_base_fetch_redacts_encoded_and_plain_token(
    tmp_path, monkeypatch, capsys
) -> None:
    seen = []

    def run(cwd, *args):
        seen.append(args)
        return subprocess.CompletedProcess(
            ["git", *args],
            128,
            "",
            "fatal: https://x-access-token:token%2Fwith%20space@github.com/example/repo.git",
        )

    monkeypatch.setattr("dorf.cli.run_git_unchecked", run)

    with pytest.raises(Exit):
        fetch_github_branch_objects_or_exit(
            repo=tmp_path / "repo",
            repo_full_name="example/repo",
            branch="main",
            token="token/with space",
        )

    error = capsys.readouterr().err
    assert seen[0][2] == ("https://x-access-token:token%2Fwith%20space@github.com/example/repo.git")
    assert "token/with space" not in error
    assert "token%2Fwith%20space" not in error
    assert "<redacted>" in error


def test_github_issue_task_keeps_workflow_contract_out_of_runtime_guidance() -> None:
    task = github_issue_task(
        "example/repo",
        GitHubIssue(42, "Fix checkout", "Body", ("Comment",)),
    )

    assert task.summary == "Issue #42: Fix checkout"
    assert "Use only the Assignment workspace named in the coding Job goal." in task.prompt
    assert "Use TDD." in task.prompt
    assert "/workspace` clone" not in task.prompt


def test_resolve_git_author_uses_repository_identity(tmp_path) -> None:
    repo = create_git_repo(tmp_path / "repo")

    identity = resolve_git_author_or_exit(repo)

    assert identity.name == "Dorf Tests"
    assert identity.email == "dorf@example.com"


@pytest.mark.parametrize(
    ("branch", "reason"),
    [
        ("main", "protected branch"),
        ("feature", "does not start with dorf/"),
        ("dorf/../main", "contains parent-directory traversal"),
        ("dorf/foo.lock", "ends with .lock"),
    ],
)
def test_dorf_branch_guardrails_reject_unsafe_branches(branch, reason) -> None:
    assert unsafe_dorf_branch_reason(branch, target_branch="develop") == reason


def test_coding_terminal_commands_end_jobs_and_retain_caller_managed_workers(
    tmp_path, monkeypatch
) -> None:
    monkeypatch.chdir(tmp_path)
    configure_passing_incus(monkeypatch)
    monkeypatch.setattr("dorf.sdk.launch_job_input_dispatcher", lambda *args: False)
    data_home = tmp_path / "data"
    env = {"XDG_DATA_HOME": str(data_home)}
    runner = CliRunner()

    for suffix in ("merged", "abandoned"):
        worker_name = f"coder-{suffix}"
        assert (
            runner.invoke(
                app,
                [
                    "worker",
                    "spawn",
                    worker_name,
                    "--provider-connection",
                    "personal-chatgpt",
                ],
                env=env,
            ).exit_code
            == 0
        )
        assert (
            runner.invoke(
                app,
                ["job", "assign", suffix, "--to", worker_name, "--goal", f"Goal {suffix}"],
                env=env,
            ).exit_code
            == 0
        )

    store = CodingStore.open(data_home / "dorf" / "state.sqlite3")
    for name in ("merged", "abandoned"):
        store.create_coding_job(
            job_name=name,
            status="ready",
            metadata={"github_repo": "example/repo"},
        )
    store.record_github_pr("merged", 42, "https://github.test/pull/42")

    class GitHub:
        def get_pull_request(self, repo, number):
            return {"state": "closed", "merged": True}

    monkeypatch.setattr("dorf.cli.github_repository_client_from_app_token", lambda: GitHub())

    completed = runner.invoke(app, ["complete", "merged"], env=env)
    discarded = runner.invoke(app, ["discard", "abandoned"], env=env)

    assert completed.exit_code == discarded.exit_code == 0
    assert store.get_coding_job("merged").status == "merged"
    assert store.get_coding_job("abandoned").status == "abandoned"
    for name in ("merged", "abandoned"):
        assert store.get_job(name).status == "ended"
        assert store.get_assignment(name).status == "ended"
        assert store.get_worker(f"coder-{name}").status == "ready"
        assert store.get_current_room(f"coder-{name}") is not None
        assert store.get_worker_binding(f"coder-{name}").room.status == "ready"


def test_version() -> None:
    result = CliRunner().invoke(app, ["--version"])

    assert result.exit_code == 0
    assert "0.1.2" in result.output


def test_resolve_git_author_rejects_empty_value(tmp_path, monkeypatch) -> None:
    repo = create_git_repo(tmp_path / "repo")
    git(repo, "config", "user.name", "   ")
    monkeypatch.setenv("GIT_CONFIG_GLOBAL", str(tmp_path / "missing-global"))

    with pytest.raises(Exit) as error:
        resolve_git_author_or_exit(repo)

    assert error.value.exit_code == 1
