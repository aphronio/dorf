"""Local-first control plane for durable AI Workers."""

from typing import TYPE_CHECKING, Any

__version__ = "0.1.0"

if TYPE_CHECKING:
    from .sdk import Dorf

__all__ = ["Dorf", "__version__"]


def __getattr__(name: str) -> Any:
    if name == "Dorf":
        from .sdk import Dorf

        return Dorf
    raise AttributeError(f"module {__name__!r} has no attribute {name!r}")
