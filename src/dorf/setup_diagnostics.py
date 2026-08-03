"""Portable, secret-safe diagnostics for the guided core setup path."""

from __future__ import annotations

import json
import os
import re
from dataclasses import asdict, dataclass, replace
from datetime import UTC, datetime
from pathlib import Path

_SECRET_ASSIGNMENT = re.compile(
    r"(?i)\b("
    r"authorization|api[_-]?key|access[_-]?token|refresh[_-]?token|"
    r"route[_-]?key|password|secret"
    r")(\s*[:=]\s*)([^\s,;]+)"
)
_BEARER_TOKEN = re.compile(r"(?i)\bbearer\s+[A-Za-z0-9._~+/=-]+")
_OPENAI_KEY = re.compile(r"\bsk-[A-Za-z0-9_-]{8,}\b")
_URL_SECRET = re.compile(r"(?i)([?&](?:token|key|code|secret|password)=)[^&\s]+")


@dataclass(frozen=True)
class SetupDiagnostic:
    """Stable semantic fields shared by human and agent setup consumers."""

    status: str
    owner: str
    classification: str
    summary: str
    observed: tuple[str, ...]
    expected: tuple[str, ...]
    safe_actions: tuple[str, ...]
    approval_required_actions: tuple[str, ...] = ()
    reproducer: tuple[str, ...] = ("dorf setup",)
    diagnostic_path: str = ""


def write_setup_diagnostic(
    diagnostic: SetupDiagnostic,
    *,
    state_home: Path | None = None,
    now: datetime | None = None,
) -> Path:
    """Write one bounded Markdown/JSON/log bundle and return its directory."""
    sanitized = sanitize_setup_diagnostic(diagnostic)
    dorf_state = (state_home or _default_state_home()) / "dorf"
    dorf_state.mkdir(parents=True, exist_ok=True)
    dorf_state.chmod(0o700)
    root = dorf_state / "diagnostics"
    root.mkdir(exist_ok=True)
    root.chmod(0o700)
    observed_at = (now or datetime.now(UTC)).astimezone(UTC)
    stem = observed_at.strftime("%Y%m%dT%H%M%S.%fZ")
    bundle = _create_bundle_directory(root, stem)
    with_path = replace(sanitized, diagnostic_path=str(bundle))
    _write_private(
        bundle / "diagnostic.json",
        json.dumps(asdict(with_path), indent=2, sort_keys=True) + "\n",
    )
    _write_private(bundle / "diagnostic.md", _render_markdown(with_path))
    _write_private(
        bundle / "commands.log",
        (
            "No raw command transcript was collected for this setup result.\n"
            "Reproducer: dorf setup\n"
        ),
    )
    return bundle


def sanitize_setup_diagnostic(diagnostic: SetupDiagnostic) -> SetupDiagnostic:
    """Redact credential-shaped values before rendering or persistence."""
    return SetupDiagnostic(
        status=_redact(diagnostic.status),
        owner=_redact(diagnostic.owner),
        classification=_redact(diagnostic.classification),
        summary=_redact(diagnostic.summary),
        observed=tuple(_redact(item) for item in diagnostic.observed),
        expected=tuple(_redact(item) for item in diagnostic.expected),
        safe_actions=tuple(_redact(item) for item in diagnostic.safe_actions),
        approval_required_actions=tuple(
            _redact(item) for item in diagnostic.approval_required_actions
        ),
        reproducer=tuple(_redact(item) for item in diagnostic.reproducer),
    )


def _redact(value: str) -> str:
    redacted = _BEARER_TOKEN.sub("Bearer [REDACTED]", value)
    redacted = _SECRET_ASSIGNMENT.sub(
        lambda match: f"{match.group(1)}{match.group(2)}[REDACTED]",
        redacted,
    )
    redacted = _OPENAI_KEY.sub("[REDACTED]", redacted)
    return _URL_SECRET.sub(r"\1[REDACTED]", redacted)


def _render_markdown(diagnostic: SetupDiagnostic) -> str:
    sections = [
        "# Dorf setup diagnostic",
        "",
        f"- Status: `{diagnostic.status}`",
        f"- Owner: `{diagnostic.owner}`",
        f"- Classification: `{diagnostic.classification}`",
        f"- Bundle: `{diagnostic.diagnostic_path}`",
        "",
        "## Summary",
        "",
        diagnostic.summary,
        "",
        *_markdown_list("Observed", diagnostic.observed),
        *_markdown_list("Expected", diagnostic.expected),
        *_markdown_list("Safe actions", diagnostic.safe_actions),
        *_markdown_list(
            "Actions requiring human approval",
            diagnostic.approval_required_actions,
        ),
        *_markdown_list("Reproducer", diagnostic.reproducer),
    ]
    return "\n".join(sections)


def _markdown_list(title: str, values: tuple[str, ...]) -> list[str]:
    lines = [f"## {title}", ""]
    lines.extend(f"- {value}" for value in values)
    if not values:
        lines.append("- None")
    lines.append("")
    return lines


def _create_bundle_directory(root: Path, stem: str) -> Path:
    for suffix in range(100):
        name = stem if suffix == 0 else f"{stem}-{suffix}"
        candidate = root / name
        try:
            candidate.mkdir(mode=0o700)
        except FileExistsError:
            continue
        return candidate
    raise OSError("Could not allocate a unique Dorf diagnostic directory")


def _write_private(path: Path, content: str) -> None:
    path.write_text(content)
    path.chmod(0o600)


def _default_state_home() -> Path:
    configured = os.environ.get("XDG_STATE_HOME")
    if configured:
        return Path(configured)
    return Path.home() / ".local" / "state"
