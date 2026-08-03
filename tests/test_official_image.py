import hashlib
import io
import json
import subprocess
import sys
from pathlib import Path
from urllib.request import Request

import pytest

from dorf.official_image import (
    OfficialImageError,
    OfficialImageInstaller,
)

PROJECT_ROOT = Path(__file__).parents[1]


class FakeResponse(io.BytesIO):
    def __init__(self, content: bytes) -> None:
        super().__init__(content)
        self.headers = {"Content-Length": str(len(content))}

    def __enter__(self):
        return self

    def __exit__(self, *exc_info) -> None:
        self.close()


class FakeOpener:
    def __init__(self, responses: dict[str, bytes]) -> None:
        self.responses = responses
        self.requested: list[str] = []

    def __call__(self, request: Request, *, timeout: float):
        url = request.full_url
        self.requested.append(url)
        return FakeResponse(self.responses[url])


class FakeIncusProbe:
    def __init__(self, fingerprint: str | None = None) -> None:
        self.fingerprint = fingerprint
        self.ran: list[list[str]] = []

    def run(self, argv, *, input=None, timeout_seconds=None):
        self.ran.append(argv)
        if argv[:3] == ["incus", "image", "list"]:
            images = (
                []
                if self.fingerprint is None
                else [
                    {
                        "aliases": [{"name": "dorf-codex"}],
                        "architecture": "x86_64",
                        "fingerprint": self.fingerprint,
                        "type": "virtual-machine",
                    }
                ]
            )
            return subprocess.CompletedProcess(argv, 0, json.dumps(images), "")
        if argv[:3] == ["incus", "image", "import"]:
            self.fingerprint = hashlib.sha256(Path(argv[3]).read_bytes()).hexdigest()
            return subprocess.CompletedProcess(argv, 0, "", "")
        raise AssertionError(argv)


def release_fixture(
    archive: bytes,
    *,
    immutable: bool = True,
    archive_digest: str | None = None,
    image_fingerprint: str | None = None,
) -> tuple[dict[str, bytes], str]:
    architecture = "x86_64"
    tag = "room-image-20260731-0.150.0"
    archive_name = f"dorf-codex-{architecture}.tar.gz"
    manifest_name = f"dorf-codex-{architecture}.json"
    digest = hashlib.sha256(archive).hexdigest()
    manifest = json.dumps(
        {
            "schema_version": 1,
            "release_tag": tag,
            "architecture": architecture,
            "image_type": "virtual-machine",
            "image_fingerprint": image_fingerprint or digest,
            "archive": {
                "name": archive_name,
                "sha256": archive_digest or digest,
                "size": len(archive),
            },
            "codex": {
                "version": "0.150.0",
                "npm_integrity": "sha512-test-integrity",
            },
            "source_commit": "a" * 40,
            "validated_at": "2026-07-31T08:30:00Z",
        },
        sort_keys=True,
    ).encode()
    manifest_digest = hashlib.sha256(manifest).hexdigest()
    manifest_url = f"https://github.com/aphronio/dorf/releases/download/{tag}/{manifest_name}"
    archive_url = f"https://github.com/aphronio/dorf/releases/download/{tag}/{archive_name}"
    api_url = "https://api.github.com/repos/aphronio/dorf/releases?per_page=30"
    releases = json.dumps(
        [
            {
                "tag_name": tag,
                "draft": False,
                "prerelease": False,
                "immutable": immutable,
                "assets": [
                    {
                        "name": manifest_name,
                        "size": len(manifest),
                        "digest": f"sha256:{manifest_digest}",
                        "browser_download_url": manifest_url,
                    },
                    {
                        "name": archive_name,
                        "size": len(archive),
                        "digest": f"sha256:{archive_digest or digest}",
                        "browser_download_url": archive_url,
                    },
                ],
            }
        ]
    ).encode()
    return {
        api_url: releases,
        manifest_url: manifest,
        archive_url: archive,
    }, digest


def test_installer_imports_only_an_immutable_digest_verified_vm_image(tmp_path) -> None:
    responses, digest = release_fixture(b"incus-vm-image")
    opener = FakeOpener(responses)
    probe = FakeIncusProbe()

    result = OfficialImageInstaller(
        probe=probe,
        opener=opener,
        architecture="x86_64",
        temp_root=tmp_path,
    ).ensure()

    assert result.status == "installed"
    assert result.release_tag == "room-image-20260731-0.150.0"
    assert result.fingerprint == digest
    assert result.codex_version == "0.150.0"
    assert ["incus", "image", "import"] == probe.ran[1][:3]
    assert probe.ran[1][-3:] == ["--alias", "dorf-codex", "--reuse"]
    assert not list(tmp_path.iterdir())


def test_installer_reuses_the_exact_promoted_fingerprint_without_downloading_archive(
    tmp_path,
) -> None:
    responses, digest = release_fixture(b"incus-vm-image")
    opener = FakeOpener(responses)
    probe = FakeIncusProbe(digest)

    result = OfficialImageInstaller(
        probe=probe,
        opener=opener,
        architecture="x86_64",
        temp_root=tmp_path,
    ).ensure()

    assert result.status == "already-ready"
    assert not any(command[:3] == ["incus", "image", "import"] for command in probe.ran)
    assert not any(url.endswith(".tar.gz") for url in opener.requested)


@pytest.mark.parametrize(
    ("fixture_kwargs", "message"),
    [
        ({"immutable": False}, "not immutable"),
        ({"archive_digest": "0" * 64}, "archive digest"),
        ({"image_fingerprint": "1" * 64}, "fingerprint"),
    ],
)
def test_installer_rejects_untrusted_release_metadata(
    tmp_path,
    fixture_kwargs,
    message,
) -> None:
    responses, _ = release_fixture(b"incus-vm-image", **fixture_kwargs)

    with pytest.raises(OfficialImageError, match=message):
        OfficialImageInstaller(
            probe=FakeIncusProbe(),
            opener=FakeOpener(responses),
            architecture="x86_64",
            temp_root=tmp_path,
        ).ensure()


def test_installer_rejects_corrupt_download_before_incus_import(tmp_path) -> None:
    responses, _ = release_fixture(b"incus-vm-image")
    archive_url = next(url for url in responses if url.endswith(".tar.gz"))
    responses[archive_url] = b"tampered-image"
    probe = FakeIncusProbe()

    with pytest.raises(OfficialImageError, match="download digest"):
        OfficialImageInstaller(
            probe=probe,
            opener=FakeOpener(responses),
            architecture="x86_64",
            temp_root=tmp_path,
        ).ensure()

    assert not any(command[:3] == ["incus", "image", "import"] for command in probe.ran)


def test_manifest_publisher_records_the_exact_export_and_validated_codex(tmp_path) -> None:
    archive = tmp_path / "dorf-codex-x86_64.tar.gz"
    archive.write_bytes(b"incus-vm-image")
    metadata = tmp_path / "image.json"
    metadata.write_text(
        json.dumps(
            {
                "package": "@openai/codex",
                "version": "0.150.0",
                "npm_integrity": "sha512-published-package",
            }
        )
    )
    output = tmp_path / "dorf-codex-x86_64.json"

    result = subprocess.run(
        [
            sys.executable,
            str(PROJECT_ROOT / "scripts" / "incus" / "create-dorf-codex-manifest.py"),
            "--archive",
            str(archive),
            "--image-metadata",
            str(metadata),
            "--release-tag",
            "room-image-20260731-0.150.0",
            "--source-commit",
            "a" * 40,
            "--validated-at",
            "2026-07-31T08:30:00Z",
            "--output",
            str(output),
        ],
        text=True,
        capture_output=True,
        check=False,
    )

    assert result.returncode == 0, result.stderr
    manifest = json.loads(output.read_text())
    digest = hashlib.sha256(archive.read_bytes()).hexdigest()
    assert manifest["image_fingerprint"] == digest
    assert manifest["archive"] == {
        "name": archive.name,
        "sha256": digest,
        "size": archive.stat().st_size,
    }
    assert manifest["codex"] == {
        "version": "0.150.0",
        "npm_integrity": "sha512-published-package",
    }
