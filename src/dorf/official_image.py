"""Verified consumption of the promoted credential-free Dorf Room image."""

from __future__ import annotations

import hashlib
import json
import platform
import re
import shutil
import tempfile
from collections.abc import Callable
from dataclasses import dataclass
from pathlib import Path
from typing import Any, Literal
from urllib.request import Request, urlopen

from dorf.adapters.environments import IncusRunnerProbe
from dorf.adapters.environments.incus import DEFAULT_INCUS_TEMPLATE

OFFICIAL_IMAGE_RELEASES_API = "https://api.github.com/repos/aphronio/dorf/releases?per_page=30"
OFFICIAL_IMAGE_RELEASE_PREFIX = "room-image-"
MAX_RELEASE_RESPONSE_BYTES = 1024 * 1024
MAX_MANIFEST_BYTES = 64 * 1024
MAX_IMAGE_BYTES = 2_000_000_000
_SHA256_PATTERN = re.compile(r"^[0-9a-f]{64}$")
_COMMIT_PATTERN = re.compile(r"^[0-9a-f]{40}$")


class OfficialImageError(RuntimeError):
    """The promoted official image could not be trusted or installed."""


@dataclass(frozen=True)
class OfficialImageArchive:
    name: str
    sha256: str
    size: int
    download_url: str


@dataclass(frozen=True)
class OfficialImageManifest:
    release_tag: str
    architecture: str
    image_fingerprint: str
    archive: OfficialImageArchive
    codex_version: str
    codex_npm_integrity: str
    source_commit: str
    validated_at: str


@dataclass(frozen=True)
class OfficialImageInstallResult:
    status: Literal["already-ready", "installed"]
    release_tag: str
    fingerprint: str
    codex_version: str


class OfficialImageInstaller:
    """Resolve one immutable release and converge its exact VM image locally."""

    def __init__(
        self,
        *,
        probe: IncusRunnerProbe | None = None,
        opener: Callable[..., Any] = urlopen,
        architecture: str | None = None,
        api_url: str = OFFICIAL_IMAGE_RELEASES_API,
        temp_root: Path | None = None,
    ) -> None:
        self._probe = probe or IncusRunnerProbe()
        self._opener = opener
        self._architecture = architecture or _host_architecture()
        self._api_url = api_url
        self._temp_root = temp_root

    def ensure(self, *, alias: str = DEFAULT_INCUS_TEMPLATE) -> OfficialImageInstallResult:
        manifest = self._latest_manifest()
        current_fingerprint = self._local_fingerprint(alias)
        if current_fingerprint == manifest.image_fingerprint:
            return OfficialImageInstallResult(
                "already-ready",
                manifest.release_tag,
                manifest.image_fingerprint,
                manifest.codex_version,
            )

        temporary_directory = Path(tempfile.mkdtemp(prefix="dorf-image.", dir=self._temp_root))
        try:
            archive_path = temporary_directory / manifest.archive.name
            self._download_file(
                manifest.archive.download_url,
                archive_path,
                expected_sha256=manifest.archive.sha256,
                expected_size=manifest.archive.size,
            )
            imported = self._probe.run(
                [
                    "incus",
                    "image",
                    "import",
                    str(archive_path),
                    "--alias",
                    alias,
                    "--reuse",
                ],
                timeout_seconds=600,
            )
            if imported.returncode != 0:
                detail = (imported.stderr or imported.stdout).strip()
                raise OfficialImageError(
                    f"Incus could not import the official Room image: {detail or 'unknown error'}"
                )
            observed = self._local_fingerprint(alias)
            if observed != manifest.image_fingerprint:
                raise OfficialImageError(
                    "Imported official image fingerprint does not match its manifest"
                )
        finally:
            shutil.rmtree(temporary_directory)

        return OfficialImageInstallResult(
            "installed",
            manifest.release_tag,
            manifest.image_fingerprint,
            manifest.codex_version,
        )

    def _latest_manifest(self) -> OfficialImageManifest:
        releases = self._read_json(self._api_url, max_bytes=MAX_RELEASE_RESPONSE_BYTES)
        if not isinstance(releases, list):
            raise OfficialImageError("Official image release response must be a list")
        release = next(
            (
                item
                for item in releases
                if isinstance(item, dict)
                and isinstance(item.get("tag_name"), str)
                and item["tag_name"].startswith(OFFICIAL_IMAGE_RELEASE_PREFIX)
                and item.get("draft") is False
                and item.get("prerelease") is False
            ),
            None,
        )
        if release is None:
            raise OfficialImageError("No promoted official Dorf Room image was found")
        tag = release["tag_name"]
        if release.get("immutable") is not True:
            raise OfficialImageError(f"Official image release {tag} is not immutable")

        manifest_name = f"dorf-codex-{self._architecture}.json"
        manifest_asset = _release_asset(release, manifest_name)
        manifest_bytes = self._download_bytes(
            manifest_asset["browser_download_url"],
            max_bytes=MAX_MANIFEST_BYTES,
        )
        manifest_digest = _asset_sha256(manifest_asset, label="manifest")
        if hashlib.sha256(manifest_bytes).hexdigest() != manifest_digest:
            raise OfficialImageError("Official image manifest digest does not match GitHub")
        try:
            data = json.loads(manifest_bytes)
        except json.JSONDecodeError as error:
            raise OfficialImageError(f"Official image manifest is invalid JSON: {error}") from error
        return _parse_manifest(
            data,
            release=release,
            expected_tag=tag,
            expected_architecture=self._architecture,
        )

    def _local_fingerprint(self, alias: str) -> str | None:
        listed = self._probe.run(
            ["incus", "image", "list", "--format", "json"],
            timeout_seconds=30,
        )
        if listed.returncode != 0:
            detail = (listed.stderr or listed.stdout).strip()
            raise OfficialImageError(
                f"Incus could not inspect local images: {detail or 'unknown error'}"
            )
        try:
            images = json.loads(listed.stdout)
        except json.JSONDecodeError as error:
            raise OfficialImageError(f"Incus returned invalid image metadata: {error}") from error
        if not isinstance(images, list):
            raise OfficialImageError("Incus image metadata must be a list")
        matches = [
            item
            for item in images
            if isinstance(item, dict)
            and any(
                isinstance(candidate, dict) and candidate.get("name") == alias
                for candidate in item.get("aliases", [])
            )
        ]
        if not matches:
            return None
        if len(matches) != 1:
            raise OfficialImageError(f"Incus returned multiple images for alias {alias}")
        image = matches[0]
        if (
            image.get("architecture") != self._architecture
            or image.get("type") != "virtual-machine"
        ):
            raise OfficialImageError(
                f"Existing Incus alias {alias} is not an {self._architecture} VM image"
            )
        fingerprint = image.get("fingerprint")
        if not isinstance(fingerprint, str) or not _SHA256_PATTERN.fullmatch(fingerprint):
            raise OfficialImageError(f"Incus image {alias} has an invalid fingerprint")
        return fingerprint

    def _read_json(self, url: str, *, max_bytes: int) -> object:
        content = self._download_bytes(url, max_bytes=max_bytes)
        try:
            return json.loads(content)
        except json.JSONDecodeError as error:
            raise OfficialImageError(
                f"Official image release response is invalid JSON: {error}"
            ) from error

    def _download_bytes(self, url: str, *, max_bytes: int) -> bytes:
        _validate_download_url(url)
        request = Request(
            url,
            headers={
                "Accept": "application/vnd.github+json",
                "User-Agent": "dorf-official-image",
                "X-GitHub-Api-Version": "2022-11-28",
            },
        )
        try:
            with self._opener(request, timeout=30) as response:
                _reject_large_content_length(response, max_bytes)
                content = response.read(max_bytes + 1)
        except OSError as error:
            raise OfficialImageError(
                f"Could not download official image metadata: {error}"
            ) from error
        if len(content) > max_bytes:
            raise OfficialImageError("Official image metadata exceeds its size limit")
        return content

    def _download_file(
        self,
        url: str,
        destination: Path,
        *,
        expected_sha256: str,
        expected_size: int,
    ) -> None:
        _validate_download_url(url)
        request = Request(url, headers={"User-Agent": "dorf-official-image"})
        digest = hashlib.sha256()
        size = 0
        try:
            with self._opener(request, timeout=60) as response:
                _reject_large_content_length(response, MAX_IMAGE_BYTES)
                with destination.open("xb") as output:
                    while chunk := response.read(1024 * 1024):
                        size += len(chunk)
                        if size > MAX_IMAGE_BYTES:
                            raise OfficialImageError(
                                "Official image download exceeds its size limit"
                            )
                        digest.update(chunk)
                        output.write(chunk)
        except OSError as error:
            raise OfficialImageError(f"Could not download official Room image: {error}") from error
        if size != expected_size:
            raise OfficialImageError("Official image download size does not match its manifest")
        if digest.hexdigest() != expected_sha256:
            raise OfficialImageError("Official image download digest does not match its manifest")


def _parse_manifest(
    data: object,
    *,
    release: dict[str, object],
    expected_tag: str,
    expected_architecture: str,
) -> OfficialImageManifest:
    if not isinstance(data, dict) or data.get("schema_version") != 1:
        raise OfficialImageError("Official image manifest schema_version must be 1")
    if data.get("release_tag") != expected_tag:
        raise OfficialImageError("Official image manifest release_tag does not match its release")
    if data.get("architecture") != expected_architecture:
        raise OfficialImageError(
            f"Official image manifest does not support {expected_architecture}"
        )
    if data.get("image_type") != "virtual-machine":
        raise OfficialImageError("Official image manifest is not a virtual-machine image")
    fingerprint = _required_sha256(data.get("image_fingerprint"), "image fingerprint")
    archive_data = data.get("archive")
    if not isinstance(archive_data, dict):
        raise OfficialImageError("Official image manifest archive must be an object")
    expected_archive_name = f"dorf-codex-{expected_architecture}.tar.gz"
    if archive_data.get("name") != expected_archive_name:
        raise OfficialImageError("Official image manifest archive name is invalid")
    archive_sha256 = _required_sha256(archive_data.get("sha256"), "archive digest")
    if archive_sha256 != fingerprint:
        raise OfficialImageError("Official image archive digest must equal its Incus fingerprint")
    archive_size = archive_data.get("size")
    if (
        not isinstance(archive_size, int)
        or isinstance(archive_size, bool)
        or not 0 < archive_size <= MAX_IMAGE_BYTES
    ):
        raise OfficialImageError("Official image manifest archive size is invalid")
    archive_asset = _release_asset(release, expected_archive_name)
    if _asset_sha256(archive_asset, label="archive") != archive_sha256:
        raise OfficialImageError("Official image archive digest does not match GitHub")
    if archive_asset.get("size") != archive_size:
        raise OfficialImageError("Official image archive size does not match GitHub")

    codex = data.get("codex")
    if not isinstance(codex, dict):
        raise OfficialImageError("Official image manifest codex must be an object")
    codex_version = codex.get("version")
    npm_integrity = codex.get("npm_integrity")
    if not isinstance(codex_version, str) or not codex_version:
        raise OfficialImageError("Official image manifest Codex version is invalid")
    if not isinstance(npm_integrity, str) or not npm_integrity.startswith("sha512-"):
        raise OfficialImageError("Official image manifest Codex npm integrity is invalid")
    source_commit = data.get("source_commit")
    if not isinstance(source_commit, str) or not _COMMIT_PATTERN.fullmatch(source_commit):
        raise OfficialImageError("Official image manifest source commit is invalid")
    validated_at = data.get("validated_at")
    if not isinstance(validated_at, str) or not validated_at.endswith("Z"):
        raise OfficialImageError("Official image manifest validation time is invalid")
    archive_url = archive_asset.get("browser_download_url")
    if not isinstance(archive_url, str):
        raise OfficialImageError("Official image archive download URL is invalid")
    _validate_release_asset_url(archive_url, expected_tag, expected_archive_name)
    return OfficialImageManifest(
        release_tag=expected_tag,
        architecture=expected_architecture,
        image_fingerprint=fingerprint,
        archive=OfficialImageArchive(
            expected_archive_name,
            archive_sha256,
            archive_size,
            archive_url,
        ),
        codex_version=codex_version,
        codex_npm_integrity=npm_integrity,
        source_commit=source_commit,
        validated_at=validated_at,
    )


def _release_asset(release: dict[str, object], name: str) -> dict[str, object]:
    assets = release.get("assets")
    if not isinstance(assets, list):
        raise OfficialImageError("Official image release assets must be a list")
    matches = [asset for asset in assets if isinstance(asset, dict) and asset.get("name") == name]
    if len(matches) != 1:
        raise OfficialImageError(f"Official image release must contain exactly one {name}")
    return matches[0]


def _asset_sha256(asset: dict[str, object], *, label: str) -> str:
    digest = asset.get("digest")
    if not isinstance(digest, str) or not digest.startswith("sha256:"):
        raise OfficialImageError(f"Official image {label} has no GitHub SHA-256 digest")
    return _required_sha256(digest.removeprefix("sha256:"), f"{label} digest")


def _required_sha256(value: object, label: str) -> str:
    if not isinstance(value, str) or not _SHA256_PATTERN.fullmatch(value):
        raise OfficialImageError(f"Official image {label} is invalid")
    return value


def _validate_download_url(url: str) -> None:
    if url == OFFICIAL_IMAGE_RELEASES_API:
        return
    if not url.startswith("https://github.com/aphronio/dorf/releases/download/"):
        raise OfficialImageError("Official image download URL is outside the Dorf repository")


def _validate_release_asset_url(url: str, tag: str, name: str) -> None:
    expected = f"https://github.com/aphronio/dorf/releases/download/{tag}/{name}"
    if url != expected:
        raise OfficialImageError("Official image release asset URL is invalid")


def _reject_large_content_length(response: object, max_bytes: int) -> None:
    headers = getattr(response, "headers", {})
    raw_length = headers.get("Content-Length")
    if raw_length is None:
        return
    try:
        content_length = int(raw_length)
    except ValueError:
        raise OfficialImageError("Official image response has an invalid size") from None
    if content_length > max_bytes:
        raise OfficialImageError("Official image response exceeds its size limit")


def _host_architecture() -> str:
    architecture = platform.machine().lower()
    if architecture in {"x86_64", "amd64"}:
        return "x86_64"
    raise OfficialImageError(f"Official Dorf Room images do not support {architecture}")
