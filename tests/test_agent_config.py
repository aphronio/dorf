import pytest

from dorf.adapters.agents.codex_config import (
    AgentConfigValidationError,
    CodexConfig,
    resolve_codex_config,
)


def test_fallback_preserves_current_primary_codex_behavior() -> None:
    resolved = resolve_codex_config()

    assert (resolved.model, resolved.reasoning_effort) == ("gpt-5.6-sol", "low")
    assert (resolved.model_source, resolved.reasoning_effort_source) == (
        "fallback",
        "fallback",
    )


def test_invocation_values_independently_override_repo_and_fallback() -> None:
    resolved = resolve_codex_config(
        CodexConfig(model="repo-model", reasoning_effort=None),
        reasoning_effort="high",
    )

    assert (resolved.model, resolved.reasoning_effort) == ("repo-model", "high")
    assert (resolved.model_source, resolved.reasoning_effort_source) == (
        "repo",
        "invocation",
    )


@pytest.mark.parametrize("effort", ["max", "ultra"])
def test_current_maximum_reasoning_efforts_are_accepted(effort: str) -> None:
    assert resolve_codex_config(reasoning_effort=effort).reasoning_effort == effort


@pytest.mark.parametrize("effort", ["", "extreme", "LOW"])
def test_invalid_reasoning_effort_is_rejected(effort: str) -> None:
    with pytest.raises(AgentConfigValidationError):
        resolve_codex_config(reasoning_effort=effort)
