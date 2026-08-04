import subprocess
from dataclasses import replace
from pathlib import Path

import pytest

from dorf.adapters.environments import (
    IncusConfig,
    IncusDoctor,
    IncusEnvironment,
    IncusFailure,
    IncusRunnerProbe,
    UnsafeEnvironmentIdentityError,
    incus_bridge_ipv4,
    remediation_commands,
)
from dorf.coding_workspace import (
    GitAuthorIdentity,
    coding_job_goal,
    git_clone_workspace_script,
    install_git_credentials,
    prepare_git_workspace,
)
from dorf.runtime import (
    Assignment,
    Job,
    JobBinding,
    JobConversation,
    Room,
    Worker,
    WorkerBinding,
)


def sample_git_author() -> GitAuthorIdentity:
    return GitAuthorIdentity(name="Dorf Tests", email="dorf@example.com")


def worker_binding() -> WorkerBinding:
    worker = Worker(
        1,
        "coder-demo",
        "codex",
        "coding-workflow",
        "dedicated",
        "assigned",
        None,
        "room-1",
        None,
        "created",
        "updated",
    )
    room = Room(
        "room-1",
        "coder-demo",
        "incus-vm",
        "dorf-coder-demo",
        "/workspace",
        "ready",
        None,
        {},
        "created",
        "updated",
    )
    return WorkerBinding(worker, room)


def job_binding() -> JobBinding:
    base = worker_binding()
    return JobBinding(
        Job(1, "demo-task", "open", 1, "Implement demo", "created", "updated"),
        Assignment(
            "assignment-demo",
            "demo-task",
            "coder-demo",
            1,
            "open",
            "room-1",
            "/workspace/jobs/demo-task",
            "created",
            None,
        ),
        JobConversation(
            "conversation-demo",
            "demo-task",
            None,
            "gpt-5.6-sol",
            "high",
            "idle",
            None,
            "created",
            "updated",
        ),
        base.worker,
        base.room,
    )


class FakeProbe(IncusRunnerProbe):
    def __init__(self, *, commands=None, missing=None) -> None:
        self.commands = commands or {}
        self.missing = missing or set()
        self.ran = []
        self.inputs = []
        self.attached = []

    def which(self, command):
        return None if command in self.missing else f"/usr/bin/{command}"

    def run(self, argv, *, input=None, timeout_seconds=None):
        self.ran.append(argv)
        self.inputs.append(input)
        result = self.commands.get(tuple(argv), subprocess.CompletedProcess(argv, 0, "", ""))
        if isinstance(result, list):
            return result.pop(0) if result else subprocess.CompletedProcess(argv, 0, "", "")
        return result

    def attach(self, argv):
        self.attached.append(argv)
        return subprocess.CompletedProcess(argv, 0, "", "")


def test_fast_checks_stop_after_missing_incus_command() -> None:
    probe = FakeProbe(missing={"incus"})
    result = IncusDoctor(probe).fast_check(IncusConfig())
    assert [failure.code for failure in result.failures] == ["incus-missing"]


def test_config_reads_root_disk_size() -> None:
    config = IncusConfig.from_mapping(
        {
            "template": "dorf-ubuntu-docker",
            "network": "dorfbr0",
            "root_disk_size": "64GiB",
        }
    )
    assert (config.template, config.network, config.root_disk_size) == (
        "dorf-ubuntu-docker",
        "dorfbr0",
        "64GiB",
    )


def test_codex_image_build_fails_if_room_auth_inputs_enter_the_base_image() -> None:
    build_script = (
        Path(__file__).parents[1] / "scripts" / "incus" / "build-dorf-codex-image.sh"
    ).read_text()
    provision_script = (
        Path(__file__).parents[1] / "scripts" / "incus" / "provision-dorf-codex.sh"
    ).read_text()
    release_script = (
        Path(__file__).parents[1] / "scripts" / "incus" / "prepare-dorf-codex-release.sh"
    ).read_text()
    publish_script = (
        Path(__file__).parents[1]
        / "scripts"
        / "incus"
        / "publish-dorf-codex-release.sh"
    ).read_text()

    assert "test ! -e /root/.codex/auth.json" in build_script
    assert "test ! -e /root/.codex/config.toml" in build_script
    assert "test ! -e /root/.config/dorf/provider-route.key" in build_script
    assert 'test -z "${OPENAI_API_KEY:-}"' in build_script
    assert 'incus image info "$BASE_IMAGE" --vm' in build_script
    assert '"$BASE_REMOTE:$BASE_FINGERPRINT"' in build_script
    assert r"""sed -n 's/.*"version": "\([^"]*\)".*/\1/p'""" in build_script
    assert "npm view @openai/codex@latest version" in provision_script
    assert 'npm install -g "@openai/codex@$CODEX_VERSION"' in provision_script
    assert "npm_integrity" in provision_script
    assert "droid" not in provision_script.lower()
    assert "validate-dorf-codex-image.py" in release_script
    assert "--preflight-only" in release_script
    assert release_script.index("--preflight-only") < release_script.index(
        'IMAGE_ALIAS="$CANDIDATE_ALIAS"'
    )
    assert 'incus image export "$CANDIDATE_ALIAS"' in release_script
    assert "create-dorf-codex-manifest.py" in release_script
    assert "prepare-dorf-codex-release.sh" in publish_script
    assert 'gh api "repos/$GITHUB_REPOSITORY" --jq .visibility' in publish_script
    assert "DORF_IMMUTABLE_RELEASES_ENABLED" in publish_script
    assert 'gh release create "$RELEASE_TAG"' in publish_script
    assert 'gh release edit "$RELEASE_TAG"' in publish_script
    assert "gh release verify-asset" in publish_script


def test_incus_bridge_address_is_resolved_from_the_selected_managed_network() -> None:
    probe = FakeProbe(
        commands={
            (
                "incus",
                "network",
                "get",
                "dorfbr0",
                "ipv4.address",
            ): subprocess.CompletedProcess([], 0, "10.125.18.1/24\n", "")
        }
    )

    assert incus_bridge_ipv4("dorfbr0", probe=probe) == "10.125.18.1"


def test_incus_environment_creates_worker_room_without_coding_policy() -> None:
    probe = FakeProbe()
    environment = IncusEnvironment(
        IncusConfig("dorf-ubuntu-docker", "dorfbr0", "64GiB"),
        probe=probe,
        sleep=lambda seconds: None,
    )
    environment.create(worker_binding())
    assert [
        "incus",
        "init",
        "dorf-ubuntu-docker",
        "dorf-coder-demo",
        "--vm",
        "--network",
        "dorfbr0",
        "-d",
        "root,size=64GiB",
    ] in probe.ran
    assert ["incus", "exec", "dorf-coder-demo", "--", "mkdir", "-p", "/workspace"] in probe.ran
    assert not any("git" in command or "codex" in command for command in probe.ran)


def test_incus_attachment_opens_interactive_shell_at_worker_workspace() -> None:
    probe = FakeProbe()

    exit_code = IncusEnvironment(probe=probe).attach(worker_binding(), cwd="/workspace")

    assert exit_code == 0
    assert probe.attached == [
        [
            "incus",
            "exec",
            "dorf-coder-demo",
            "--cwd",
            "/workspace",
            "--mode",
            "interactive",
            "--",
            "bash",
        ]
    ]


def test_incus_attachment_preserves_interactive_shell_exit_status() -> None:
    probe = FakeProbe()
    probe.attach = lambda argv: subprocess.CompletedProcess(argv, 7, None, None)

    exit_code = IncusEnvironment(probe=probe).attach(worker_binding(), cwd="/workspace")

    assert exit_code == 7


def test_incus_environment_routes_job_commands_through_worker_room() -> None:
    probe = FakeProbe()
    environment = IncusEnvironment(probe=probe)
    binding = job_binding()
    result = environment.execute(
        binding,
        ["git", "status"],
        cwd=binding.workspace,
        env={"GIT_TERMINAL_PROMPT": "0"},
    )
    assert result.returncode == 0
    assert probe.ran == [
        [
            "incus",
            "exec",
            "dorf-coder-demo",
            "--cwd",
            "/workspace/jobs/demo-task",
            "--",
            "env",
            "GIT_TERMINAL_PROMPT=0",
            "git",
            "status",
        ]
    ]


def test_incus_environment_pulls_regular_file_without_following_links(tmp_path) -> None:
    class PullProbe(FakeProbe):
        def pull_file(self, argv, destination, *, max_bytes):
            self.ran.append(argv)
            destination.write_bytes(b"profile")
            return subprocess.CompletedProcess(argv, 0, "", "")

    probe = PullProbe()
    destination = tmp_path / "profile.txt"
    IncusEnvironment(probe=probe).pull_file(
        job_binding(),
        "/run/dorf/jobs/demo-task/outbox/new/report-profile/files/0001",
        destination,
        max_bytes=16,
    )
    assert destination.read_bytes() == b"profile"
    assert "O_NOFOLLOW" in probe.ran[-1][6]


def test_incus_recovery_restarts_the_exact_stopped_worker_room() -> None:
    info = subprocess.CompletedProcess([], 0, "Status: STOPPED\n", "")
    probe = FakeProbe(commands={("incus", "info", "dorf-coder-demo"): info})

    outcome = IncusEnvironment(probe=probe, sleep=lambda seconds: None).restore(worker_binding())

    assert outcome == "restored"
    assert ["incus", "start", "dorf-coder-demo"] in probe.ran
    assert ["incus", "exec", "dorf-coder-demo", "--", "true"] in probe.ran


def test_incus_cleanup_uses_exact_worker_provider_identity() -> None:
    missing = subprocess.CompletedProcess([], 1, "", "Error: Instance not found")
    probe = FakeProbe(commands={("incus", "info", "dorf-coder-demo"): missing})
    assert IncusEnvironment(probe=probe).stop(worker_binding()) == "absent"
    assert probe.ran == [["incus", "info", "dorf-coder-demo"]]


@pytest.mark.parametrize("provider_id", ["", "dorf-someone-else"])
def test_incus_cleanup_never_guesses_unknown_room_identity(provider_id) -> None:
    probe = FakeProbe()
    binding = worker_binding()
    unsafe = WorkerBinding(binding.worker, replace(binding.room, provider_id=provider_id))
    environment = IncusEnvironment(probe=probe)
    with pytest.raises(UnsafeEnvironmentIdentityError, match="Room"):
        environment.stop(unsafe)
    with pytest.raises(UnsafeEnvironmentIdentityError, match="Room"):
        environment.destroy(unsafe)
    assert probe.ran == []


def test_incus_execution_never_guesses_missing_room_provider_identity() -> None:
    probe = FakeProbe()
    binding = job_binding()
    unsafe = replace(binding, room=replace(binding.room, provider_id=""))

    with pytest.raises(UnsafeEnvironmentIdentityError, match="Room"):
        IncusEnvironment(probe=probe).execute(unsafe, ["true"])

    assert probe.ran == []


def test_job_credential_refresh_uses_assignment_seam_and_redacts_token() -> None:
    token = "installation-secret-token"
    calls = []

    class FailingEnvironment:
        def execute(self, binding, argv, **kwargs):
            calls.append((binding, argv, kwargs))
            return subprocess.CompletedProcess(argv, 1, "", f"failed with {token}")

    binding = job_binding()
    with pytest.raises(RuntimeError) as raised:
        install_git_credentials(FailingEnvironment(), binding, token=token)

    assert len(calls) == 1
    actual_binding, argv, options = calls[0]
    assert actual_binding == binding
    assert options["cwd"] == "/workspace/jobs/demo-task"
    assert options["input"] == f"{token}\n"
    assert token not in " ".join(argv)
    assert token not in str(raised.value)
    assert "<redacted>" in str(raised.value)


def test_coding_goal_names_exact_job_workspace_and_pr_contract() -> None:
    prompt = coding_job_goal(
        job_name="demo-task",
        task="Demo task",
        job_branch="dorf/demo-task",
        workspace="/workspace/jobs/demo-task",
    )
    assert "Work only in /workspace/jobs/demo-task." in prompt
    assert "Push HEAD to origin dorf/demo-task" in prompt
    assert "pr_title: <draft PR title>" in prompt


def test_git_clone_script_requires_fresh_job_workspace_and_never_uses_worktree() -> None:
    script = git_clone_workspace_script(
        "https://github.com/example/repo.git",
        "dorf/demo-task",
        "/workspace/jobs/demo-task",
    )
    assert 'test -z "$(find /workspace/jobs/demo-task' in script
    assert "IFS= read -r GITHUB_TOKEN" in script
    assert "git clone" in script
    assert "git worktree" not in script
    assert "rm -rf" not in script
    assert subprocess.run(["bash", "-n"], input=script, text=True).returncode == 0


def test_git_workspace_fails_when_normal_auth_check_fails() -> None:
    binding = job_binding()
    auth = (
        "incus",
        "exec",
        "dorf-coder-demo",
        "--cwd",
        binding.workspace,
        "--",
        "env",
        "GIT_TERMINAL_PROMPT=0",
        "git",
        "ls-remote",
        "--heads",
        "https://github.com/example/repo.git",
        "dorf/demo-task",
    )
    probe = FakeProbe(
        commands={auth: subprocess.CompletedProcess([], 128, "", "fatal: authentication failed")}
    )
    with pytest.raises(RuntimeError, match="Job Git credentials"):
        prepare_git_workspace(
            IncusEnvironment(probe=probe),
            binding,
            repo_full_name="example/repo",
            token="installation-token",
            branch="dorf/demo-task",
            git_author=sample_git_author(),
        )


def test_incus_environment_waits_for_guest_agent() -> None:
    check = ("incus", "exec", "dorf-coder-demo", "--", "true")
    probe = FakeProbe(
        commands={
            check: [
                subprocess.CompletedProcess([], 1, "", "not ready"),
                subprocess.CompletedProcess([], 0, "", ""),
            ]
        }
    )
    IncusEnvironment(probe=probe, sleep=lambda seconds: None).create(worker_binding())
    assert probe.ran.count(list(check)) == 2


def test_fast_checks_verify_network_and_template_inputs() -> None:
    probe = FakeProbe(
        commands={
            ("incus", "info"): subprocess.CompletedProcess(["incus", "info"], 0, "", ""),
            ("incus", "network", "show", "missingbr0"): subprocess.CompletedProcess(
                ["incus", "network", "show", "missingbr0"], 1, "", "not found"
            ),
            ("incus", "image", "info", "missing-template"): subprocess.CompletedProcess(
                ["incus", "image", "info", "missing-template"], 1, "", "not found"
            ),
        }
    )

    result = IncusDoctor(probe).fast_check(
        IncusConfig(network="missingbr0", template="missing-template")
    )

    assert [failure.code for failure in result.failures] == [
        "incus-network",
        "incus-template",
    ]
    assert ["incus", "network", "show", "missingbr0"] in probe.ran
    assert ["incus", "image", "info", "missing-template"] in probe.ran


def test_deep_doctor_builds_probe_vm_commands_and_cleans_up() -> None:
    probe = FakeProbe()

    result = IncusDoctor(probe).deep_check(
        IncusConfig(template="images:ubuntu/24.04", network="incusbr0"),
        probe_name="dorf-incus-doctor-test",
    )

    assert result.ok is True
    assert ["incus", "network", "show", "incusbr0"] in probe.ran
    assert ["incus", "image", "info", "images:ubuntu/24.04"] in probe.ran
    assert [
        "incus",
        "launch",
        "images:ubuntu/24.04",
        "dorf-incus-doctor-test",
        "--vm",
        "--network",
        "incusbr0",
    ] in probe.ran
    assert [
        "incus",
        "exec",
        "dorf-incus-doctor-test",
        "--",
        "bash",
        "-lc",
        "ip -4 route get 1.1.1.1",
    ] in probe.ran
    assert [
        "incus",
        "exec",
        "dorf-incus-doctor-test",
        "--",
        "bash",
        "-lc",
        (
            "apt-get update && "
            "DEBIAN_FRONTEND=noninteractive apt-get install -y "
            "docker.io docker-compose-v2"
        ),
    ] in probe.ran
    assert probe.ran[-1] == [
        "incus",
        "delete",
        "dorf-incus-doctor-test",
        "--force",
    ]


def test_core_doctor_checks_room_connectivity_without_installing_coding_tools() -> None:
    probe = FakeProbe()

    result = IncusDoctor(probe).core_check(
        IncusConfig(),
        probe_name="dorf-core-doctor-test",
    )

    assert result.ok is True
    assert [
        "incus",
        "exec",
        "dorf-core-doctor-test",
        "--",
        "bash",
        "-lc",
        "ip -4 route get 1.1.1.1",
    ] in probe.ran
    assert not any(
        command[:3] == ["incus", "exec", "dorf-core-doctor-test"] and "apt-get" in " ".join(command)
        for command in probe.ran
    )
    assert probe.ran[-1] == [
        "incus",
        "delete",
        "dorf-core-doctor-test",
        "--force",
    ]


def test_deep_doctor_launches_probe_on_configured_network() -> None:
    probe = FakeProbe()

    result = IncusDoctor(probe).deep_check(
        IncusConfig(network="dorfbr0"),
        probe_name="probe",
    )

    assert result.ok is True
    assert [
        "incus",
        "launch",
        "dorf-codex",
        "probe",
        "--vm",
        "--network",
        "dorfbr0",
    ] in probe.ran


def test_deep_doctor_waits_for_guest_agent_before_guest_checks() -> None:
    agent_check = ("incus", "exec", "probe", "--", "true")
    dhcp_check = (
        "incus",
        "exec",
        "probe",
        "--",
        "bash",
        "-lc",
        "ip -4 route get 1.1.1.1",
    )
    probe = FakeProbe(
        commands={
            agent_check: [
                subprocess.CompletedProcess([], 1, "", "agent not ready"),
                subprocess.CompletedProcess([], 0, "", ""),
            ]
        }
    )

    result = IncusDoctor(probe, sleep=lambda seconds: None).deep_check(
        IncusConfig(),
        probe_name="probe",
    )

    assert result.ok is True
    assert probe.ran.index(list(agent_check)) < probe.ran.index(list(dhcp_check))
    assert probe.ran.count(list(agent_check)) == 2


def test_deep_doctor_reports_guest_agent_timeout_without_guest_checks() -> None:
    agent_check = ("incus", "exec", "probe", "--", "true")
    dhcp_check = [
        "incus",
        "exec",
        "probe",
        "--",
        "bash",
        "-lc",
        "ip -4 route get 1.1.1.1",
    ]
    probe = FakeProbe(
        commands={
            agent_check: [
                subprocess.CompletedProcess([], 1, "", "agent not ready") for _ in range(30)
            ]
        }
    )

    result = IncusDoctor(probe, sleep=lambda seconds: None).deep_check(
        IncusConfig(),
        probe_name="probe",
    )

    assert result.ok is False
    assert [failure.code for failure in result.failures] == ["guest-agent"]
    assert dhcp_check not in probe.ran


def test_deep_doctor_stops_at_fast_check_failures() -> None:
    probe = FakeProbe(missing={"incus"})

    result = IncusDoctor(probe).deep_check(IncusConfig(), probe_name="probe")

    assert result.ok is False
    assert result.failures[0].code == "incus-missing"
    assert "sudo pacman" in result.remediation
    assert [
        "incus",
        "launch",
        "images:ubuntu/24.04",
        "probe",
        "--vm",
        "--network",
        "incusbr0",
    ] not in probe.ran


def test_deep_doctor_classifies_guest_failures_and_keeps_cleanup_non_fatal() -> None:
    probe = FakeProbe(
        commands={
            (
                "incus",
                "exec",
                "probe",
                "--",
                "bash",
                "-lc",
                "ip -4 route get 1.1.1.1",
            ): subprocess.CompletedProcess([], 1, "", "network unreachable"),
            ("incus", "delete", "probe", "--force"): subprocess.CompletedProcess(
                [], 1, "", "not found"
            ),
        }
    )

    result = IncusDoctor(probe).deep_check(IncusConfig(), probe_name="probe")

    assert result.ok is False
    assert result.failures[0].code == "guest-dhcpv4"
    assert "sudo ufw allow in on incusbr0 proto udp from any port 68 to any port 67" in (
        result.remediation
    )
    assert probe.ran[-1] == ["incus", "delete", "probe", "--force"]


def test_deep_doctor_classifies_docker_install_failure_without_runtime_cascade() -> None:
    install_command = (
        "apt-get update && "
        "DEBIAN_FRONTEND=noninteractive apt-get install -y "
        "docker.io docker-compose-v2"
    )
    probe = FakeProbe(
        commands={
            (
                "incus",
                "exec",
                "probe",
                "--",
                "bash",
                "-lc",
                install_command,
            ): subprocess.CompletedProcess([], 1, "", "package install failed"),
        }
    )

    result = IncusDoctor(probe).deep_check(IncusConfig(), probe_name="probe")

    assert result.ok is False
    assert [failure.code for failure in result.failures] == ["guest-docker-install"]
    assert [
        "incus",
        "exec",
        "probe",
        "--",
        "bash",
        "-lc",
        "docker --version && docker info",
    ] not in probe.ran


def test_deep_doctor_does_not_delete_explicit_probe_name_when_launch_fails() -> None:
    probe = FakeProbe(
        commands={
            (
                "incus",
                "launch",
                "dorf-codex",
                "probe",
                "--vm",
                "--network",
                "incusbr0",
            ): subprocess.CompletedProcess([], 1, "", "Instance name already exists")
        }
    )

    result = IncusDoctor(probe).deep_check(IncusConfig(), probe_name="probe")

    assert result.ok is False
    assert result.failures[0].code == "probe-launch"
    assert ["incus", "delete", "probe", "--force"] not in probe.ran


def test_remediation_output_includes_detected_egress_interface() -> None:
    output = remediation_commands(
        [
            IncusFailure("guest-dhcpv4", "missing DHCP"),
            IncusFailure("guest-outbound-tcp", "blocked TCP"),
        ],
        network="incusbr0",
        egress_interface="wlan0",
    )

    assert "sudo ufw allow in on incusbr0 proto udp from any port 68 to any port 67" in output
    assert "sudo ufw route allow in on incusbr0 out on wlan0" in output
