import tomllib
from pathlib import Path

ROOT = Path(__file__).parents[1]


def test_replaced_python_terminal_is_absent_but_provisioning_assets_remain() -> None:
    replaced = (
        "src/dorf/cli.py",
        "src/dorf/sdk.py",
        "src/dorf/codex_room.py",
        "src/dorf/coding_workspace.py",
        "src/dorf/adapters/agents/codex.py",
        "src/dorf/adapters/environments/incus_environment.py",
        "src/dorf/job_input_dispatcher.py",
        "src/dorf/worker_message_dispatcher.py",
        "src/dorf/report_collector.py",
    )
    assert not [path for path in replaced if (ROOT / path).exists()]

    project = tomllib.loads((ROOT / "pyproject.toml").read_text())["project"]
    assert "scripts" not in project
    assert not hasattr(__import__("dorf"), "Dorf")

    retained = (
        "scripts/incus/build-dorf-codex-image.sh",
        "scripts/incus/provision-dorf-codex.sh",
        "scripts/incus/prepare-dorf-codex-release.sh",
        "scripts/incus/validate-dorf-coding-workstation.py",
    )
    assert all((ROOT / path).is_file() for path in retained)
    validator = (ROOT / retained[-1]).read_text()
    assert "dorf.sdk" not in validator
    assert '"admit"' in validator and '"cleanup"' in validator
