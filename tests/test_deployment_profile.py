import json
import stat

import pytest

from dorf.adapters.environments import IncusConfig
from dorf.deployment_profile import (
    DeploymentProfile,
    DeploymentProfileError,
    load_deployment_profile,
    save_deployment_profile,
)


def test_deployment_profile_round_trips_without_duplicating_credentials(tmp_path) -> None:
    profile = DeploymentProfile(
        provider_connection="personal-chatgpt",
        incus=IncusConfig(
            template="dorf-codex-validated",
            network="dorfbr0",
            root_disk_size="60GiB",
        ),
        image_fingerprint="a" * 64,
    )

    path = save_deployment_profile(profile, config_home=tmp_path)

    assert path == tmp_path / "dorf" / "deployment.json"
    assert stat.S_IMODE(path.stat().st_mode) == 0o600
    assert load_deployment_profile(config_home=tmp_path) == profile
    persisted = json.loads(path.read_text())
    assert persisted == {
        "environment": "incus",
        "incus": {
            "fingerprint": "a" * 64,
            "network": "dorfbr0",
            "root_disk_size": "60GiB",
            "template": "dorf-codex-validated",
        },
        "provider_connection": "personal-chatgpt",
    }
    assert "credential" not in path.read_text().lower()


def test_deployment_profile_rejects_unknown_or_incomplete_configuration(tmp_path) -> None:
    path = tmp_path / "dorf" / "deployment.json"
    path.parent.mkdir()
    path.write_text('{"environment": "docker", "provider_connection": ""}\n')

    with pytest.raises(
        DeploymentProfileError,
        match="environment must be 'incus'",
    ):
        load_deployment_profile(config_home=tmp_path)
