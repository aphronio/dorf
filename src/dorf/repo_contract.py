from __future__ import annotations

import re
import tomllib
from dataclasses import dataclass, field
from pathlib import Path

from dorf.adapters.agents.codex_config import CodexConfig, validate_codex_config
from dorf.verifier_tooling import PI_REASONING_EFFORTS

CONTRACT_FILENAME = ".dorf.toml"
DEFAULT_REVIEW_TIMEOUT_SECONDS = 1800
REVIEW_PROMPT_PLACEHOLDER = "{dorf_review_prompt}"
VERIFIER_ROLE_HARNESS = "pi"
VERIFIER_ROLE_AUTHORITIES = frozenset({"shadow", "advisory"})
VERIFIER_ROLE_ROOM = "dedicated"
VERIFIER_MODEL_PATTERN = re.compile(r"^[A-Za-z0-9][A-Za-z0-9._-]*$")


class ContractValidationError(ValueError):
    """Raised when a repo contract exists but cannot be used."""


@dataclass(frozen=True)
class ReviewAgent:
    name: str
    command: str
    enabled: bool = True


@dataclass(frozen=True)
class ReviewConfig:
    max_rounds: int = 1
    timeout_seconds: int = DEFAULT_REVIEW_TIMEOUT_SECONDS
    prompt: str = ""
    agents: dict[str, ReviewAgent] = field(default_factory=dict)


@dataclass(frozen=True)
class VerifierRole:
    """One typed isolated verification role with no repository-owned host commands."""

    name: str
    harness: str
    connection: str
    model: str
    reasoning_effort: str
    authority: str
    room: str
    timeout_seconds: int
    prompt: str


@dataclass(frozen=True)
class RepoContract:
    mode: str
    commands: dict[str, str]
    env: dict[str, str]
    review: ReviewConfig | None = None
    incus_config: dict[str, str] = field(default_factory=dict)
    primary_codex: CodexConfig | None = None
    verifier_roles: dict[str, VerifierRole] = field(default_factory=dict)


def load_repo_contract(repo: Path) -> RepoContract:
    contract_path = repo / CONTRACT_FILENAME
    if not contract_path.exists():
        return RepoContract(
            mode="generic",
            commands={},
            env={},
            review=ReviewConfig(),
        )

    try:
        data = tomllib.loads(contract_path.read_text())
    except tomllib.TOMLDecodeError as error:
        raise ContractValidationError(str(error)) from error

    if "runner" in data:
        raise ContractValidationError(
            "[runner] is not supported; configure the concrete [incus] environment"
        )

    commands = data.get("commands", {})
    if not isinstance(commands, dict):
        raise ContractValidationError("[commands] must be a table")

    parsed_commands: dict[str, str] = {}
    for name, command in commands.items():
        if not isinstance(command, str):
            raise ContractValidationError(f"commands.{name} must be a string")
        parsed_commands[name] = command

    agent = data.get("agent", {})
    if agent is None:
        agent = {}
    if not isinstance(agent, dict):
        raise ContractValidationError("[agent] must be a table")
    codex = agent.get("codex")
    primary_codex = None
    if codex is not None:
        if not isinstance(codex, dict):
            raise ContractValidationError("[agent.codex] must be a table")
        model = codex.get("model")
        reasoning_effort = codex.get("reasoning_effort")
        if model is not None and not isinstance(model, str):
            raise ContractValidationError("agent.codex.model must be a string")
        if reasoning_effort is not None and not isinstance(reasoning_effort, str):
            raise ContractValidationError("agent.codex.reasoning_effort must be a string")
        try:
            validate_codex_config(model, reasoning_effort)
        except ValueError as error:
            raise ContractValidationError(str(error)) from error
        primary_codex = CodexConfig(model, reasoning_effort)

    review = data.get("review", {})
    if review is None:
        review = {}
    if not isinstance(review, dict):
        raise ContractValidationError("[review] must be a table")

    max_rounds = review.get("max_rounds", 1)
    if not isinstance(max_rounds, int):
        raise ContractValidationError("review.max_rounds must be an integer")
    if max_rounds < 1:
        raise ContractValidationError("review.max_rounds must be at least 1")
    timeout_seconds = review.get("timeout_seconds", DEFAULT_REVIEW_TIMEOUT_SECONDS)
    if not isinstance(timeout_seconds, int):
        raise ContractValidationError("review.timeout_seconds must be an integer")
    if timeout_seconds < 1:
        raise ContractValidationError("review.timeout_seconds must be at least 1")
    prompt = review.get("prompt", "")
    if not isinstance(prompt, str):
        raise ContractValidationError("review.prompt must be a string")
    review_agents = review.get("agents", {})
    if review_agents is None:
        review_agents = {}
    if not isinstance(review_agents, dict):
        raise ContractValidationError("[review.agents] must be a table")

    parsed_review_agents: dict[str, ReviewAgent] = {}
    for name, agent in review_agents.items():
        if not isinstance(agent, dict):
            raise ContractValidationError(f"review.agents.{name} must be a table")
        command = agent.get("command")
        if not isinstance(command, str):
            raise ContractValidationError(f"review.agents.{name}.command must be a string")
        enabled = agent.get("enabled", True)
        if not isinstance(enabled, bool):
            raise ContractValidationError(f"review.agents.{name}.enabled must be a boolean")
        parsed_review_agents[name] = ReviewAgent(
            name=name,
            command=command,
            enabled=enabled,
        )

    env = data.get("env", {})
    if not isinstance(env, dict):
        raise ContractValidationError("[env] must be a table")

    parsed_env: dict[str, str] = {}
    for name, binding in env.items():
        if not isinstance(binding, dict):
            raise ContractValidationError(f"env.{name} must be a table")
        source = binding.get("source")
        if not isinstance(source, str):
            raise ContractValidationError(f"env.{name}.source must be a string")
        parsed_env[name] = source

    verification = data.get("verification", {})
    if verification is None:
        verification = {}
    if not isinstance(verification, dict):
        raise ContractValidationError("[verification] must be a table")
    roles = verification.get("roles", {})
    if roles is None:
        roles = {}
    if not isinstance(roles, dict):
        raise ContractValidationError("[verification.roles] must be a table")
    parsed_roles: dict[str, VerifierRole] = {}
    for name, role in roles.items():
        parsed_roles[name] = _parse_verifier_role(name, role)

    incus = data.get("incus", {})
    if incus is None:
        incus = {}
    if not isinstance(incus, dict):
        raise ContractValidationError("[incus] must be a table")
    parsed_incus_config: dict[str, str] = {}
    for key, value in incus.items():
        if not isinstance(value, str):
            raise ContractValidationError(f"incus.{key} must be a string")
        parsed_incus_config[key] = value

    return RepoContract(
        mode="configured",
        commands=parsed_commands,
        env=parsed_env,
        review=ReviewConfig(
            max_rounds=max_rounds,
            timeout_seconds=timeout_seconds,
            prompt=prompt,
            agents=parsed_review_agents,
        ),
        incus_config=parsed_incus_config,
        primary_codex=primary_codex,
        verifier_roles=parsed_roles,
    )


def _parse_verifier_role(name: str, role: object) -> VerifierRole:
    """Parse one typed verification role without permitting repository-owned commands."""
    if not isinstance(role, dict):
        raise ContractValidationError(f"verification.roles.{name} must be a table")
    if "command" in role or "commands" in role:
        raise ContractValidationError(
            f"verification.roles.{name} must not declare repository-owned commands"
        )
    harness = role.get("harness")
    if harness != VERIFIER_ROLE_HARNESS:
        raise ContractValidationError(
            f"verification.roles.{name}.harness must be {VERIFIER_ROLE_HARNESS!r}"
        )
    connection = role.get("connection")
    if not isinstance(connection, str) or not connection.strip():
        raise ContractValidationError(
            f"verification.roles.{name}.connection must be a provider connection name"
        )
    connection = connection.strip()
    if re.fullmatch(r"[a-z][a-z0-9]*(?:-[a-z0-9]+)*", connection) is None:
        raise ContractValidationError(
            f"verification.roles.{name}.connection must use lowercase letters, "
            "numbers, and single hyphens"
        )
    model = role.get("model")
    if not isinstance(model, str) or not model.strip():
        raise ContractValidationError(f"verification.roles.{name}.model must be a string")
    model = model.strip()
    if VERIFIER_MODEL_PATTERN.fullmatch(model) is None:
        raise ContractValidationError(
            f"verification.roles.{name}.model must contain only letters, "
            "numbers, '.', '_' and '-'"
        )
    reasoning_effort = role.get("reasoning_effort")
    if (
        not isinstance(reasoning_effort, str)
        or reasoning_effort not in PI_REASONING_EFFORTS
    ):
        choices = ", ".join(sorted(PI_REASONING_EFFORTS))
        raise ContractValidationError(
            f"verification.roles.{name}.reasoning_effort must be one of: {choices}"
        )
    authority = role.get("authority")
    if authority not in VERIFIER_ROLE_AUTHORITIES:
        choices = ", ".join(sorted(VERIFIER_ROLE_AUTHORITIES))
        raise ContractValidationError(
            f"verification.roles.{name}.authority must be one of: {choices}"
        )
    room = role.get("room")
    if room != VERIFIER_ROLE_ROOM:
        raise ContractValidationError(
            f"verification.roles.{name}.room must be {VERIFIER_ROLE_ROOM!r}"
        )
    timeout_seconds = role.get("timeout_seconds", DEFAULT_REVIEW_TIMEOUT_SECONDS)
    if not isinstance(timeout_seconds, int) or timeout_seconds < 1:
        raise ContractValidationError(
            f"verification.roles.{name}.timeout_seconds must be a positive integer"
        )
    prompt = role.get("prompt", "")
    if not isinstance(prompt, str):
        raise ContractValidationError(f"verification.roles.{name}.prompt must be a string")
    return VerifierRole(
        name=name,
        harness=harness,
        connection=connection,
        model=model,
        reasoning_effort=reasoning_effort,
        authority=authority,
        room=room,
        timeout_seconds=timeout_seconds,
        prompt=prompt,
    )
