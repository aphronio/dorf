from pathlib import Path

import pytest

from dorf.repo_contract import ContractValidationError, load_repo_contract


def test_dorf_repo_declares_contract() -> None:
    repo = Path(__file__).resolve().parents[1]

    contract = load_repo_contract(repo)

    assert contract.mode == "configured"
    assert isinstance(contract.commands.get("check"), str)
    assert isinstance(contract.commands.get("smoke"), str)
    assert contract.commands.get("prepare") == (
        "UV_CACHE_DIR=.dorf/uv-cache uv sync --frozen --all-groups"
    )
    assert "review" not in contract.commands
    assert contract.review is not None
    assert contract.review.timeout_seconds == 1800
    assert contract.review.agents
    assert any(agent.enabled for agent in contract.review.agents.values())
    assert all(agent.command for agent in contract.review.agents.values())
    assert all("{dorf_review_prompt}" in agent.command for agent in contract.review.agents.values())
    assert contract.review.agents["codex"].enabled is True
    assert "2>/dev/null" not in contract.review.agents["codex"].command
    assert contract.review.agents["droid"].enabled is False
    assert "kimi-k2.7-code" in contract.review.agents["droid"].command
    assert contract.incus_config.get("template") == "dorf-codex"
    assert contract.primary_codex is not None
    assert contract.primary_codex.model == "gpt-5.6-sol"
    assert contract.primary_codex.reasoning_effort == "low"
    assert contract.env == {}


def test_missing_contract_loads_generic_mode(tmp_path: Path) -> None:
    contract = load_repo_contract(tmp_path)

    assert contract.mode == "generic"
    assert contract.commands == {}
    assert contract.review is not None
    assert contract.review.agents == {}
    assert contract.review.timeout_seconds == 1800


def test_valid_contract_loads_configured_commands(tmp_path: Path) -> None:
    (tmp_path / ".dorf.toml").write_text(
        """
[commands]
bootstrap = "mise run agent:bootstrap"
check = "uv run pytest"
smoke = "mise run agent:smoke"

[env]
PATH = { source = "host.PATH" }
JOB = { source = "dorf.job_name" }
""".strip()
    )

    contract = load_repo_contract(tmp_path)

    assert contract.mode == "configured"
    assert contract.commands == {
        "bootstrap": "mise run agent:bootstrap",
        "check": "uv run pytest",
        "smoke": "mise run agent:smoke",
    }
    assert contract.env == {
        "PATH": "host.PATH",
        "JOB": "dorf.job_name",
    }


def test_valid_contract_loads_partial_primary_codex_defaults(tmp_path: Path) -> None:
    (tmp_path / ".dorf.toml").write_text('[agent.codex]\nreasoning_effort = "high"\n')

    contract = load_repo_contract(tmp_path)

    assert contract.primary_codex is not None
    assert contract.primary_codex.model is None
    assert contract.primary_codex.reasoning_effort == "high"


@pytest.mark.parametrize(
    ("content", "message"),
    [
        ('[agent.codex]\nmodel = "bad model"\n', "Codex model"),
        ('[agent.codex]\nreasoning_effort = "extreme"\n', "Codex reasoning effort"),
    ],
)
def test_invalid_primary_codex_configuration_fails_contract(
    tmp_path: Path, content: str, message: str
) -> None:
    (tmp_path / ".dorf.toml").write_text(content)

    with pytest.raises(ContractValidationError, match=message):
        load_repo_contract(tmp_path)


def test_valid_contract_loads_review_agents(tmp_path: Path) -> None:
    (tmp_path / ".dorf.toml").write_text(
        """
[review]
max_rounds = 3
timeout_seconds = 900
prompt = "Use the house review rubric."

[review.agents.codex]
enabled = false
command = "codex review --base main"

[review.agents.droid]
command = "droid exec review"
""".strip()
    )

    contract = load_repo_contract(tmp_path)

    assert contract.review is not None
    assert contract.review.max_rounds == 3
    assert contract.review.timeout_seconds == 900
    assert contract.review.prompt == "Use the house review rubric."
    assert contract.review.agents is not None
    assert contract.review.agents["codex"].enabled is False
    assert contract.review.agents["codex"].command == "codex review --base main"
    assert contract.review.agents["droid"].enabled is True
    assert contract.review.agents["droid"].command == "droid exec review"


def test_valid_contract_loads_the_concrete_incus_environment(tmp_path: Path) -> None:
    (tmp_path / ".dorf.toml").write_text(
        """
[incus]
template = "dorf-ubuntu-docker"
root_disk_size = "40GiB"
""".strip()
    )

    contract = load_repo_contract(tmp_path)

    assert contract.incus_config == {
        "template": "dorf-ubuntu-docker",
        "root_disk_size": "40GiB",
    }


def test_provider_style_runner_configuration_is_not_a_supported_matrix(
    tmp_path: Path,
) -> None:
    (tmp_path / ".dorf.toml").write_text('[runner.incus-vm]\ntemplate = "dorf-ubuntu-docker"\n')

    with pytest.raises(ContractValidationError, match=r"\[runner\].*\[incus\]"):
        load_repo_contract(tmp_path)


def test_invalid_contract_requires_commands_table(tmp_path: Path) -> None:
    (tmp_path / ".dorf.toml").write_text('commands = "uv run pytest"\n')

    with pytest.raises(ContractValidationError, match=r"\[commands\] must be a table"):
        load_repo_contract(tmp_path)


def test_invalid_contract_requires_string_commands(tmp_path: Path) -> None:
    (tmp_path / ".dorf.toml").write_text(
        """
[commands]
check = ["uv", "run", "pytest"]
""".strip()
    )

    with pytest.raises(ContractValidationError, match="commands.check must be a string"):
        load_repo_contract(tmp_path)


def test_invalid_contract_requires_review_table(tmp_path: Path) -> None:
    (tmp_path / ".dorf.toml").write_text('review = "droid"\n')

    with pytest.raises(ContractValidationError, match=r"\[review\] must be a table"):
        load_repo_contract(tmp_path)


def test_invalid_contract_requires_positive_review_max_rounds(tmp_path: Path) -> None:
    (tmp_path / ".dorf.toml").write_text("[review]\nmax_rounds = 0\n")

    with pytest.raises(ContractValidationError, match="review.max_rounds must be at least 1"):
        load_repo_contract(tmp_path)


def test_invalid_contract_requires_positive_review_timeout(tmp_path: Path) -> None:
    (tmp_path / ".dorf.toml").write_text("[review]\ntimeout_seconds = 0\n")

    with pytest.raises(ContractValidationError, match="review.timeout_seconds must be at least 1"):
        load_repo_contract(tmp_path)


def test_invalid_contract_requires_review_prompt_string(tmp_path: Path) -> None:
    (tmp_path / ".dorf.toml").write_text('[review]\nprompt = ["review"]\n')

    with pytest.raises(
        ContractValidationError,
        match="review.prompt must be a string",
    ):
        load_repo_contract(tmp_path)


def test_invalid_contract_requires_review_agent_command(tmp_path: Path) -> None:
    (tmp_path / ".dorf.toml").write_text(
        """
[review.agents.droid]
enabled = true
""".strip()
    )

    with pytest.raises(
        ContractValidationError,
        match="review.agents.droid.command must be a string",
    ):
        load_repo_contract(tmp_path)


def test_invalid_contract_requires_review_agent_enabled_bool(tmp_path: Path) -> None:
    (tmp_path / ".dorf.toml").write_text(
        """
[review.agents.droid]
enabled = "yes"
command = "droid exec review"
""".strip()
    )

    with pytest.raises(
        ContractValidationError,
        match="review.agents.droid.enabled must be a boolean",
    ):
        load_repo_contract(tmp_path)


def test_invalid_contract_requires_env_table(tmp_path: Path) -> None:
    (tmp_path / ".dorf.toml").write_text('env = "PATH"\n')

    with pytest.raises(ContractValidationError, match=r"\[env\] must be a table"):
        load_repo_contract(tmp_path)


def test_invalid_contract_requires_env_source_strings(tmp_path: Path) -> None:
    (tmp_path / ".dorf.toml").write_text(
        """
[env]
PATH = { source = 123 }
""".strip()
    )

    with pytest.raises(ContractValidationError, match="env.PATH.source must be a string"):
        load_repo_contract(tmp_path)
