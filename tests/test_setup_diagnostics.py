import json
import stat
from datetime import UTC, datetime

from dorf.setup_diagnostics import (
    SetupDiagnostic,
    write_setup_diagnostic,
)


def test_setup_diagnostic_bundle_is_private_structured_and_agent_readable(
    tmp_path,
) -> None:
    diagnostic = SetupDiagnostic(
        status="paused",
        owner="dorf",
        classification="configuration",
        summary="The official Room image is not available locally.",
        observed=("No image matched dorf-codex.",),
        expected=("One validated VM image is available.",),
        safe_actions=("Rerun setup after the public image release.",),
    )

    bundle = write_setup_diagnostic(
        diagnostic,
        state_home=tmp_path,
        now=datetime(2026, 7, 31, 12, 30, tzinfo=UTC),
    )

    assert bundle.parent == tmp_path / "dorf" / "diagnostics"
    assert {path.name for path in bundle.iterdir()} == {
        "commands.log",
        "diagnostic.json",
        "diagnostic.md",
    }
    assert stat.S_IMODE(bundle.stat().st_mode) == 0o700
    assert all(stat.S_IMODE(path.stat().st_mode) == 0o600 for path in bundle.iterdir())
    payload = json.loads((bundle / "diagnostic.json").read_text())
    assert payload["diagnostic_path"] == str(bundle)
    assert payload["safe_actions"] == ["Rerun setup after the public image release."]
    markdown = (bundle / "diagnostic.md").read_text()
    assert "# Dorf setup diagnostic" in markdown
    assert "Actions requiring human approval" in markdown


def test_setup_diagnostic_redacts_credentials_from_every_written_format(
    tmp_path,
) -> None:
    diagnostic = SetupDiagnostic(
        status="failed",
        owner="provider-gateway",
        classification="configuration",
        summary="OPENAI_API_KEY=sk-proj-supersecretvalue failed",
        observed=("Authorization: Bearer ey.secret.value",),
        expected=("callback?code=device-secret&safe=yes",),
        safe_actions=("password=hunter2",),
    )

    bundle = write_setup_diagnostic(diagnostic, state_home=tmp_path)
    combined = "\n".join(path.read_text() for path in bundle.iterdir())

    for secret in (
        "sk-proj-supersecretvalue",
        "ey.secret.value",
        "device-secret",
        "hunter2",
    ):
        assert secret not in combined
    assert "[REDACTED]" in combined
