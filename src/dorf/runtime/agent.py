"""Harness-neutral conversation observations and failures."""

from __future__ import annotations

from dataclasses import dataclass
from typing import Any


@dataclass(frozen=True)
class AgentConversationInspection:
    """Native conversation data returned transiently for one inspection."""

    connection_status: str
    native: dict[str, Any]
    attention_status: str | None = None


class AgentInspectionError(RuntimeError):
    category = "inspection"


class AgentUnavailableError(AgentInspectionError):
    category = "agent-unavailable"


class ConversationMissingError(AgentInspectionError):
    category = "conversation-missing"


@dataclass(frozen=True)
class AgentTurnRecovery:
    """Harness observation used to reconcile one unsettled durable turn."""

    status: str
    native_turn_id: str | None = None
    error: str | None = None


class AgentDriverError(RuntimeError):
    exit_code = 126
    category = "driver"

    def diagnostic(self) -> str:
        return f"agent {self.category} failure: {self}"
