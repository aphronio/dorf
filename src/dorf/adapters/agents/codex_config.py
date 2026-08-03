"""Configuration for the built-in Codex agent adapter."""

from __future__ import annotations

import re
from dataclasses import dataclass

DEFAULT_CODEX_MODEL = "gpt-5.6-sol"
DEFAULT_CODEX_REASONING_EFFORT = "low"
CODEX_REASONING_EFFORTS = frozenset({"low", "medium", "high", "xhigh", "max", "ultra"})
_MODEL_PATTERN = re.compile(r"^[A-Za-z0-9][A-Za-z0-9._-]*$")


class AgentConfigValidationError(ValueError):
    pass


@dataclass(frozen=True)
class CodexConfig:
    model: str | None
    reasoning_effort: str | None


@dataclass(frozen=True)
class ResolvedCodexConfig(CodexConfig):
    model_source: str
    reasoning_effort_source: str


def validate_codex_config(model: str | None, reasoning_effort: str | None) -> None:
    if model is not None and not _MODEL_PATTERN.fullmatch(model):
        raise AgentConfigValidationError(
            "Codex model must contain only letters, numbers, '.', '_' and '-'"
        )
    if reasoning_effort is not None and reasoning_effort not in CODEX_REASONING_EFFORTS:
        choices = ", ".join(sorted(CODEX_REASONING_EFFORTS))
        raise AgentConfigValidationError(f"Codex reasoning effort must be one of: {choices}")


def resolve_codex_config(
    repo: CodexConfig | None = None,
    *,
    model: str | None = None,
    reasoning_effort: str | None = None,
) -> ResolvedCodexConfig:
    validate_codex_config(model, reasoning_effort)
    resolved_model = model or (repo.model if repo and repo.model else DEFAULT_CODEX_MODEL)
    resolved_reasoning = reasoning_effort or (
        repo.reasoning_effort if repo and repo.reasoning_effort else DEFAULT_CODEX_REASONING_EFFORT
    )
    validate_codex_config(resolved_model, resolved_reasoning)
    return ResolvedCodexConfig(
        model=resolved_model,
        reasoning_effort=resolved_reasoning,
        model_source="invocation" if model else ("repo" if repo and repo.model else "fallback"),
        reasoning_effort_source=(
            "invocation"
            if reasoning_effort
            else ("repo" if repo and repo.reasoning_effort else "fallback")
        ),
    )
