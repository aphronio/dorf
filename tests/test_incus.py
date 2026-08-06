import importlib.util
import json
import subprocess
import sys
from pathlib import Path

from dorf.adapters.environments import (
    IncusConfig,
    IncusDoctor,
    IncusFailure,
    IncusRunnerProbe,
    incus_bridge_ipv4,
    remediation_commands,
)

PROJECT_ROOT = Path(__file__).parents[1]


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
    workstation_validator = (
        Path(__file__).parents[1] / "scripts" / "incus" / "validate-dorf-coding-workstation.py"
    ).read_text()
    publish_script = (
        Path(__file__).parents[1]
        / "scripts"
        / "incus"
        / "publish-dorf-codex-release.sh"
    ).read_text()
    credential_check = (
        Path(__file__).parents[1]
        / "scripts"
        / "incus"
        / "assert-dorf-codex-credential-free.sh"
    ).read_text()

    assert "assert-dorf-codex-credential-free.sh" in build_script
    assert ".codex/auth.json" in credential_check
    assert ".config/gh/hosts.yml" in credential_check
    assert ".factory" in credential_check
    assert "OPENAI_API_KEY" in credential_check
    assert "DEEPSEEK_API_KEY" in credential_check
    assert "GITHUB_TOKEN" in credential_check
    assert "FACTORY_API_KEY" in credential_check
    assert 'incus image info "$BASE_IMAGE" --vm' in build_script
    assert '"$BASE_REMOTE:$BASE_FINGERPRINT"' in build_script
    assert r"""sed -n 's/.*"version": "\([^"]*\)".*/\1/p'""" in build_script
    assert "npm view @openai/codex@latest version" in provision_script
    assert 'npm install -g "@openai/codex@$CODEX_VERSION"' in provision_script
    assert 'NODE_VERSION="22.23.2"' in provision_script
    assert 'npm install -g "@earendil-works/pi-coding-agent@$PI_VERSION"' in provision_script
    assert "git" in provision_script
    assert "astral.sh/uv/install.sh" not in provision_script
    assert "uv-x86_64-unknown-linux-gnu.tar.gz" in provision_script
    assert "90b2f223fb69d19db49e117da601f64978593417988530aa733d456141b4bcbb" in (
        provision_script
    )
    assert "sha256sum --check --strict" in provision_script
    assert "npm_integrity" in provision_script
    assert "droid" not in provision_script.lower()
    assert "validate-dorf-codex-image.py" in release_script
    assert "--preflight-only" in release_script
    assert release_script.index("trap cleanup EXIT") < release_script.index("--preflight-only")
    assert release_script.index("--preflight-only") < release_script.index(
        'IMAGE_ALIAS="$CANDIDATE_ALIAS"'
    )
    assert 'incus image export "$CANDIDATE_ALIAS"' in release_script
    assert "dorf-codex-incus-vm-v3-x86_64" in release_script
    assert 'test -x "$(command -v git)"' in release_script
    assert "! command -v npm" in release_script
    assert 'test -x "$(command -v uv)"' in release_script
    assert "validate-dorf-coding-workstation.py" in release_script
    assert '["go", "build"' in workstation_validator
    for command in ("admit", "worker", "inspect", "cleanup"):
        assert f'"{command}"' in workstation_validator
    assert "dorf.sdk" not in workstation_validator
    assert "dorf.runtime" not in workstation_validator
    assert ".dorf.toml" not in workstation_validator
    assert "finally:" in workstation_validator
    assert '"--cancel-run"' in workstation_validator
    assert '"--now"' in workstation_validator
    assert 'incus delete "$VALIDATION_VM" --force' in release_script
    cleanup = release_script.split("cleanup() {", 1)[1].split("}\ntrap cleanup EXIT", 1)[0]
    assert '[[ "$EVIDENCE_POLICY" == "remove" ]]' in cleanup
    assert 'rm -rf -- "$EVIDENCE_DIR"' in cleanup
    assert "create-dorf-codex-manifest.py" in release_script
    assert "prepare-dorf-codex-release.sh" in publish_script
    assert "dorf-codex-incus-vm-v3-x86_64" in publish_script
    assert 'gh api "repos/$GITHUB_REPOSITORY" --jq .visibility' in publish_script
    assert "DORF_IMMUTABLE_RELEASES_ENABLED" in publish_script
    assert 'gh release create "$RELEASE_TAG"' in publish_script
    assert 'gh release edit "$RELEASE_TAG"' in publish_script
    assert "gh release verify-asset" in publish_script


def test_image_credential_check_rejects_owner_files_and_environment(tmp_path) -> None:
    script = (
        Path(__file__).parents[1]
        / "scripts"
        / "incus"
        / "assert-dorf-codex-credential-free.sh"
    )
    clean_env = {
        "PATH": "/usr/bin:/bin",
        "DORF_IMAGE_HOME": str(tmp_path),
    }

    clean = subprocess.run([str(script)], env=clean_env, text=True, capture_output=True)
    credential = tmp_path / ".config" / "gh" / "hosts.yml"
    credential.parent.mkdir(parents=True)
    credential.write_text("oauth_token: owner-secret\n")
    github = subprocess.run([str(script)], env=clean_env, text=True, capture_output=True)
    credential.unlink()
    factory = subprocess.run(
        [str(script)],
        env={**clean_env, "FACTORY_API_KEY": "owner-secret"},
        text=True,
        capture_output=True,
    )

    assert clean.returncode == 0
    assert github.returncode == 1
    assert "hosts.yml" in github.stderr
    assert "owner-secret" not in github.stderr
    assert factory.returncode == 1
    assert "FACTORY_API_KEY" in factory.stderr
    assert "owner-secret" not in factory.stderr


def test_workstation_validator_checks_its_source_before_go_delivery(tmp_path) -> None:
    result = subprocess.run(
        [
            sys.executable,
            str(PROJECT_ROOT / "scripts" / "incus" / "validate-dorf-coding-workstation.py"),
            "--image",
            "candidate",
            "--image-fingerprint",
            "a" * 64,
            "--provider-connection",
            "test-provider",
            "--source-commit",
            "0" * 40,
            "--proof-id",
            "test-proof",
            "--project-root",
            str(PROJECT_ROOT),
            "--evidence-dir",
            str(tmp_path),
        ],
        text=True,
        capture_output=True,
        check=False,
    )

    assert result.returncode != 0
    assert "source commit does not match the validation checkout" in result.stderr
    assert not list(tmp_path.iterdir())


def test_candidate_preflight_names_an_available_go_remediation() -> None:
    validator = (
        PROJECT_ROOT / "scripts" / "incus" / "validate-dorf-codex-image.py"
    ).read_text()

    assert "dorf provider status" not in validator
    assert "go run ./cmd/dorf doctor --provider" in validator


def test_workstation_cleanup_uses_exact_go_fallback_after_durable_failure(monkeypatch) -> None:
    path = PROJECT_ROOT / "scripts" / "incus" / "validate-dorf-coding-workstation.py"
    spec = importlib.util.spec_from_file_location("workstation_validator", path)
    assert spec is not None and spec.loader is not None
    validator = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(validator)
    calls: list[list[str]] = []

    def fake_run(argv, *, cwd=None, env=None):
        del cwd, env
        calls.append(argv)
        if argv[1:] == ["cleanup", "job-proof"]:
            raise RuntimeError("durable cleanup unavailable")
        stdout = ""
        if argv[1:] == ["inspect", "--json", "job-proof"]:
            stdout = json.dumps({"job": {"cleanup_state": "complete"}})
        return subprocess.CompletedProcess(argv, 0, stdout, ""), 0.01

    monkeypatch.setattr(validator, "_run", fake_run)
    validator._cleanup_proof(Path("/tmp/dorf"), "job-proof", {})

    assert ["/tmp/dorf", "cleanup", "--cancel-run", "--now", "job-proof"] in calls
    assert calls[-1] == ["/tmp/dorf", "inspect", "--json", "job-proof"]


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
