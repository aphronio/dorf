import json
import os
import subprocess
import sys

from dorf.runtime.reporting import report_command_source

_REPORT_SCOPE = (
    "DORF_JOB_NAME",
    "DORF_ASSIGNMENT_ID",
    "DORF_REPORT_ROOT",
)


def report_env(**values: str) -> dict[str, str]:
    env = os.environ.copy()
    for name in _REPORT_SCOPE:
        env.pop(name, None)
    env.update(values)
    return env


def test_dorf_report_does_not_infer_job_scope_from_worker_identity(
    tmp_path,
) -> None:
    command = tmp_path / "dorf-report"
    command.write_text(report_command_source())

    result = subprocess.run(
        [
            sys.executable,
            str(command),
            "progress",
            "--summary",
            "General conversation update",
        ],
        env=report_env(
            DORF_WORKER_NAME="researcher",
            DORF_WORKSPACE="/workspace",
        ),
        text=True,
        capture_output=True,
        check=False,
    )

    assert result.returncode == 2
    assert "DORF_JOB_NAME is missing" in result.stderr


def test_dorf_report_requires_explicit_assignment_scope_before_publication(
    tmp_path,
) -> None:
    command = tmp_path / "dorf-report"
    command.write_text(report_command_source())
    outbox = tmp_path / "outbox"

    result = subprocess.run(
        [
            sys.executable,
            str(command),
            "progress",
            "--summary",
            "Found the slow query",
        ],
        env=report_env(
            DORF_JOB_NAME="checkout-perf",
            DORF_REPORT_ROOT=str(outbox),
        ),
        text=True,
        capture_output=True,
        check=False,
    )

    assert result.returncode == 2
    assert "DORF_ASSIGNMENT_ID is missing" in result.stderr
    assert not outbox.exists()


def test_dorf_report_rejects_unsafe_assignment_identity(tmp_path) -> None:
    command = tmp_path / "dorf-report"
    command.write_text(report_command_source())
    outbox = tmp_path / "outbox"

    result = subprocess.run(
        [
            sys.executable,
            str(command),
            "progress",
            "--summary",
            "Found the slow query",
        ],
        env=report_env(
            DORF_JOB_NAME="checkout-perf",
            DORF_ASSIGNMENT_ID="../../other-job",
            DORF_REPORT_ROOT=str(outbox),
        ),
        text=True,
        capture_output=True,
        check=False,
    )

    assert result.returncode == 2
    assert "DORF_ASSIGNMENT_ID is invalid" in result.stderr
    assert not outbox.exists()


def test_dorf_report_rejects_relative_outbox_scope(tmp_path) -> None:
    command = tmp_path / "dorf-report"
    command.write_text(report_command_source())

    result = subprocess.run(
        [
            sys.executable,
            str(command),
            "progress",
            "--summary",
            "Found the slow query",
        ],
        env=report_env(
            DORF_JOB_NAME="checkout-perf",
            DORF_ASSIGNMENT_ID="assignment-checkout",
            DORF_REPORT_ROOT="relative-outbox",
        ),
        cwd=tmp_path,
        text=True,
        capture_output=True,
        check=False,
    )

    assert result.returncode == 2
    assert "DORF_REPORT_ROOT must be absolute" in result.stderr
    assert not (tmp_path / "relative-outbox").exists()


def test_dorf_report_requires_explicit_outbox_scope(tmp_path) -> None:
    command = tmp_path / "dorf-report"
    command.write_text(report_command_source())

    result = subprocess.run(
        [
            sys.executable,
            str(command),
            "progress",
            "--summary",
            "Found the slow query",
        ],
        env=report_env(
            DORF_JOB_NAME="checkout-perf",
            DORF_ASSIGNMENT_ID="assignment-checkout",
        ),
        text=True,
        capture_output=True,
        check=False,
    )

    assert result.returncode == 2
    assert "DORF_REPORT_ROOT is missing" in result.stderr


def test_dorf_report_rejects_report_id_outside_collector_grammar(tmp_path) -> None:
    command = tmp_path / "dorf-report"
    command.write_text(report_command_source())
    outbox = tmp_path / "outbox"

    result = subprocess.run(
        [
            sys.executable,
            str(command),
            "progress",
            "--summary",
            "Found the slow query",
            "--id",
            "report-UPPER",
        ],
        env=report_env(
            DORF_JOB_NAME="checkout-perf",
            DORF_ASSIGNMENT_ID="assignment-checkout",
            DORF_REPORT_ROOT=str(outbox),
        ),
        text=True,
        capture_output=True,
        check=False,
    )

    assert result.returncode == 2
    assert "report ID is invalid" in result.stderr
    assert not outbox.exists()


def test_dorf_report_publishes_complete_worker_claim_atomically(tmp_path) -> None:
    command = tmp_path / "dorf-report"
    command.write_text(report_command_source())
    command.chmod(0o755)
    artifact = tmp_path / "profile.txt"
    artifact.write_text("p95=120ms\n")
    outbox = tmp_path / "outbox"

    result = subprocess.run(
        [
            sys.executable,
            str(command),
            "evidence",
            "--summary",
            "Captured checkout profile",
            "--file",
            str(artifact),
        ],
        env=report_env(
            DORF_JOB_NAME="checkout-perf",
            DORF_ASSIGNMENT_ID="assignment-checkout",
            DORF_REPORT_ROOT=str(outbox),
        ),
        text=True,
        capture_output=True,
        check=False,
    )

    assert result.returncode == 0
    report_id = result.stdout.strip()
    assert report_id.startswith("report-")
    assert list((outbox / "tmp").iterdir()) == []
    bundle = outbox / "new" / report_id
    manifest = json.loads((bundle / "manifest.json").read_text())
    assert manifest == {
        "artifacts": [
            {
                "file": "0001",
                "media_type": "text/plain",
                "name": "profile.txt",
            }
        ],
        "assignment": "assignment-checkout",
        "id": report_id,
        "job": "checkout-perf",
        "kind": "evidence",
        "schema_version": 1,
        "summary": "Captured checkout profile",
    }
    assert (bundle / "files" / "0001").read_text() == "p95=120ms\n"
