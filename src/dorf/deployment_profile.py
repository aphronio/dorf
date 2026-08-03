"""Host-local defaults for composing new Dorf Rooms."""

from __future__ import annotations

import json
import os
import re
import tempfile
from dataclasses import dataclass, replace
from pathlib import Path

from dorf.adapters.environments import IncusConfig

DEPLOYMENT_PROFILE_FILENAME = "deployment.json"
_IMAGE_FINGERPRINT_PATTERN = re.compile(r"^[0-9a-f]{64}$")


class DeploymentProfileError(ValueError):
    """Raised when the global deployment profile cannot be used."""


@dataclass(frozen=True)
class DeploymentProfile:
    provider_connection: str
    incus: IncusConfig = IncusConfig()
    image_fingerprint: str | None = None

    def __post_init__(self) -> None:
        if not self.provider_connection.strip():
            raise DeploymentProfileError("provider_connection must not be empty")
        for field in ("template", "network", "root_disk_size"):
            value = getattr(self.incus, field)
            if not isinstance(value, str) or not value.strip():
                raise DeploymentProfileError(f"incus.{field} must be a non-empty string")
        if self.image_fingerprint is not None and not _IMAGE_FINGERPRINT_PATTERN.fullmatch(
            self.image_fingerprint
        ):
            raise DeploymentProfileError("image_fingerprint must be a lowercase SHA-256")

    def save(self, *, config_home: Path | None = None) -> Path:
        return save_deployment_profile(self, config_home=config_home)

    def with_provider_connection(self, name: str) -> DeploymentProfile:
        return replace(self, provider_connection=name)


def deployment_profile_path(*, config_home: Path | None = None) -> Path:
    root = config_home or _default_config_home()
    return root / "dorf" / DEPLOYMENT_PROFILE_FILENAME


def load_deployment_profile(*, config_home: Path | None = None) -> DeploymentProfile:
    path = deployment_profile_path(config_home=config_home)
    if not path.is_file():
        raise DeploymentProfileError(
            "Dorf setup is incomplete: no global deployment profile was found. "
            "Connect a provider with `dorf provider connect --help`."
        )
    try:
        data = json.loads(path.read_text())
    except (OSError, json.JSONDecodeError) as error:
        raise DeploymentProfileError(f"Invalid deployment profile {path}: {error}") from error
    if not isinstance(data, dict):
        raise DeploymentProfileError("deployment profile must be a JSON object")
    if data.get("environment") != "incus":
        raise DeploymentProfileError("deployment profile environment must be 'incus'")

    provider_connection = data.get("provider_connection")
    if not isinstance(provider_connection, str) or not provider_connection.strip():
        raise DeploymentProfileError(
            "deployment profile provider_connection must be a non-empty string"
        )
    incus = data.get("incus")
    if not isinstance(incus, dict):
        raise DeploymentProfileError("deployment profile incus must be an object")
    expected_fields = ("template", "network", "root_disk_size")
    for field in expected_fields:
        value = incus.get(field)
        if not isinstance(value, str) or not value.strip():
            raise DeploymentProfileError(
                f"deployment profile incus.{field} must be a non-empty string"
            )
    image_fingerprint = incus.get("fingerprint")
    if image_fingerprint is not None and (
        not isinstance(image_fingerprint, str)
        or not _IMAGE_FINGERPRINT_PATTERN.fullmatch(image_fingerprint)
    ):
        raise DeploymentProfileError(
            "deployment profile incus.fingerprint must be a lowercase SHA-256"
        )
    return DeploymentProfile(
        provider_connection=provider_connection,
        incus=IncusConfig(
            template=incus["template"],
            network=incus["network"],
            root_disk_size=incus["root_disk_size"],
        ),
        image_fingerprint=image_fingerprint,
    )


def load_optional_deployment_profile(
    *,
    config_home: Path | None = None,
) -> DeploymentProfile | None:
    if not deployment_profile_path(config_home=config_home).is_file():
        return None
    return load_deployment_profile(config_home=config_home)


def save_deployment_profile(
    profile: DeploymentProfile,
    *,
    config_home: Path | None = None,
) -> Path:
    path = deployment_profile_path(config_home=config_home)
    path.parent.mkdir(parents=True, exist_ok=True)
    path.parent.chmod(0o700)
    content = json.dumps(
        {
            "environment": "incus",
            "incus": {
                **(
                    {"fingerprint": profile.image_fingerprint}
                    if profile.image_fingerprint is not None
                    else {}
                ),
                "network": profile.incus.network,
                "root_disk_size": profile.incus.root_disk_size,
                "template": profile.incus.template,
            },
            "provider_connection": profile.provider_connection,
        },
        indent=2,
        sort_keys=True,
    )
    temp_path: Path | None = None
    try:
        with tempfile.NamedTemporaryFile(
            "w",
            dir=path.parent,
            delete=False,
            prefix=f".{path.name}.",
        ) as temp_file:
            temp_path = Path(temp_file.name)
            temp_file.write(content + "\n")
            temp_file.flush()
            os.fsync(temp_file.fileno())
        temp_path.chmod(0o600)
        temp_path.replace(path)
    finally:
        if temp_path is not None:
            temp_path.unlink(missing_ok=True)
    return path


def set_default_provider_connection(
    name: str,
    *,
    config_home: Path | None = None,
) -> DeploymentProfile:
    path = deployment_profile_path(config_home=config_home)
    profile = (
        load_deployment_profile(config_home=config_home)
        if path.is_file()
        else DeploymentProfile(provider_connection=name)
    )
    updated = profile.with_provider_connection(name)
    save_deployment_profile(updated, config_home=config_home)
    return updated


def _default_config_home() -> Path:
    configured = os.environ.get("XDG_CONFIG_HOME")
    if configured:
        return Path(configured)
    return Path.home() / ".config"
