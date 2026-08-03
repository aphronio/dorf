"""Programmatic control plane for shared model-provider access."""

from .cliproxy import (
    ConsumerWireIncompatibleError,
    DeviceAuthorization,
    GatewayHealth,
    GatewayUnavailableError,
    InferenceRoute,
    ProviderAuthenticationError,
    ProviderAuthenticationStaleError,
    ProviderConnection,
    ProviderConnectionNotFoundError,
    ProviderGateway,
    ProviderGatewayError,
    ProviderSelectionUnsupportedError,
    ProviderUpstreamUnavailableError,
)

__all__ = [
    "ConsumerWireIncompatibleError",
    "DeviceAuthorization",
    "GatewayHealth",
    "GatewayUnavailableError",
    "InferenceRoute",
    "ProviderAuthenticationError",
    "ProviderAuthenticationStaleError",
    "ProviderConnection",
    "ProviderConnectionNotFoundError",
    "ProviderGateway",
    "ProviderGatewayError",
    "ProviderSelectionUnsupportedError",
    "ProviderUpstreamUnavailableError",
]
