#!/usr/bin/env python3
"""Create the canonical manifest for one validated Incus VM export."""

from __future__ import annotations

import argparse
import hashlib
import json
import re
from pathlib import Path

COMMIT_PATTERN = re.compile(r"^[0-9a-f]{40}$")


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument("--archive", type=Path, required=True)
    parser.add_argument("--image-metadata", type=Path, required=True)
    parser.add_argument("--release-tag", required=True)
    parser.add_argument("--source-commit", required=True)
    parser.add_argument("--validated-at", required=True)
    parser.add_argument("--output", type=Path, required=True)
    args = parser.parse_args()

    if args.archive.name != "dorf-codex-x86_64.tar.gz":
        parser.error("archive must be named dorf-codex-x86_64.tar.gz")
    if not args.release_tag.startswith("room-image-"):
        parser.error("release tag must start with room-image-")
    if not COMMIT_PATTERN.fullmatch(args.source_commit):
        parser.error("source commit must be a full lowercase Git SHA")
    if not args.validated_at.endswith("Z"):
        parser.error("validated-at must be a UTC timestamp ending in Z")

    metadata = json.loads(args.image_metadata.read_text())
    if metadata.get("package") != "@openai/codex":
        parser.error("image metadata package must be @openai/codex")
    codex_version = metadata.get("version")
    npm_integrity = metadata.get("npm_integrity")
    if not isinstance(codex_version, str) or not codex_version:
        parser.error("image metadata has no Codex version")
    if not isinstance(npm_integrity, str) or not npm_integrity.startswith("sha512-"):
        parser.error("image metadata has no SHA-512 npm integrity")

    archive_digest = hashlib.sha256()
    with args.archive.open("rb") as archive_file:
        while chunk := archive_file.read(1024 * 1024):
            archive_digest.update(chunk)
    fingerprint = archive_digest.hexdigest()
    manifest = {
        "schema_version": 1,
        "release_tag": args.release_tag,
        "architecture": "x86_64",
        "image_type": "virtual-machine",
        "image_fingerprint": fingerprint,
        "archive": {
            "name": args.archive.name,
            "sha256": fingerprint,
            "size": args.archive.stat().st_size,
        },
        "codex": {
            "version": codex_version,
            "npm_integrity": npm_integrity,
        },
        "source_commit": args.source_commit,
        "validated_at": args.validated_at,
    }
    args.output.write_text(json.dumps(manifest, indent=2, sort_keys=True) + "\n")


if __name__ == "__main__":
    main()
