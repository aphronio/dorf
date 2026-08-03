#!/usr/bin/env python3
"""Dependency-free Worker report command installed inside a Dorf Room."""

from __future__ import annotations

import argparse
import json
import mimetypes
import os
import re
import secrets
import shutil
import stat
import sys
import time
from pathlib import Path

REPORT_KINDS = ("progress", "assumption", "evidence", "completed")
MAX_ARTIFACTS = 20
MAX_ARTIFACT_BYTES = 100 * 1024 * 1024
MAX_SUMMARY_CHARACTERS = 4096
_JOB_NAME = re.compile(r"^[a-z0-9][a-z0-9-]{0,62}$")
_ASSIGNMENT_ID = re.compile(r"^assignment-[a-z0-9][a-z0-9-]{0,120}$")
_REPORT_ID = re.compile(r"^report-[a-z0-9][a-z0-9-]{0,120}$")


def fail(message: str) -> None:
    print(f"dorf-report: {message}", file=sys.stderr)
    raise SystemExit(2)


def parse_args(argv: list[str] | None = None) -> argparse.Namespace:
    parser = argparse.ArgumentParser(description="Publish a structured Worker claim to Dorf.")
    parser.add_argument("kind", choices=REPORT_KINDS)
    parser.add_argument("--summary", required=True)
    parser.add_argument("--file", action="append", default=[])
    parser.add_argument("--id", dest="report_id")
    return parser.parse_args(argv)


def main(argv: list[str] | None = None) -> int:
    args = parse_args(argv)
    summary = args.summary.strip()
    if (
        not summary
        or len(summary) > MAX_SUMMARY_CHARACTERS
        or any(character in summary for character in ("\n", "\r", "\x00"))
    ):
        fail("summary must be one line containing 1-4096 characters")
    if len(args.file) > MAX_ARTIFACTS:
        fail("at most 20 files may be attached")

    job = os.environ.get("DORF_JOB_NAME", "").strip()
    if not job:
        fail("DORF_JOB_NAME is missing")
    if not _JOB_NAME.fullmatch(job):
        fail("DORF_JOB_NAME is invalid")
    assignment_id = os.environ.get("DORF_ASSIGNMENT_ID", "").strip()
    if not assignment_id:
        fail("DORF_ASSIGNMENT_ID is missing")
    if not _ASSIGNMENT_ID.fullmatch(assignment_id):
        fail("DORF_ASSIGNMENT_ID is invalid")
    report_id = args.report_id or (f"report-{time.time_ns():020d}-{secrets.token_hex(16)}")
    if not _REPORT_ID.fullmatch(report_id):
        fail("report ID is invalid")

    report_root = os.environ.get("DORF_REPORT_ROOT", "").strip()
    if not report_root:
        fail("DORF_REPORT_ROOT is missing")
    root = Path(report_root)
    if not root.is_absolute():
        fail("DORF_REPORT_ROOT must be absolute")
    for child in ("tmp", "new", "acks"):
        (root / child).mkdir(parents=True, exist_ok=True, mode=0o700)
    temporary = root / "tmp" / report_id
    published = root / "new" / report_id
    if published.exists():
        print(report_id)
        return 0
    try:
        temporary.mkdir(mode=0o700)
    except FileExistsError:
        fail("report ID is already being prepared")

    artifacts: list[dict[str, str]] = []
    try:
        files_path = temporary / "files"
        files_path.mkdir(mode=0o700)
        for index, value in enumerate(args.file, start=1):
            source = Path(value)
            try:
                metadata = source.lstat()
            except OSError as error:
                fail(f"cannot read artifact {value}: {error}")
            if not stat.S_ISREG(metadata.st_mode):
                fail(f"artifact is not a regular file: {value}")

            stored_name = f"{index:04d}"
            destination = files_path / stored_name
            try:
                descriptor = os.open(source, os.O_RDONLY | getattr(os, "O_NOFOLLOW", 0))
            except OSError as error:
                fail(f"cannot open artifact {value}: {error}")
            if not stat.S_ISREG(os.fstat(descriptor).st_mode):
                os.close(descriptor)
                fail(f"artifact is not a regular file: {value}")

            size = 0
            with (
                os.fdopen(descriptor, "rb") as source_file,
                destination.open("xb") as output,
            ):
                while chunk := source_file.read(1024 * 1024):
                    size += len(chunk)
                    if size > MAX_ARTIFACT_BYTES:
                        fail(f"artifact exceeds 100 MiB: {value}")
                    output.write(chunk)
                output.flush()
                os.fsync(output.fileno())
            artifacts.append(
                {
                    "file": stored_name,
                    "media_type": mimetypes.guess_type(source.name)[0]
                    or "application/octet-stream",
                    "name": source.name,
                }
            )

        manifest = {
            "schema_version": 1,
            "id": report_id,
            "job": job,
            "assignment": assignment_id,
            "kind": args.kind,
            "summary": summary,
            "artifacts": artifacts,
        }
        manifest_path = temporary / "manifest.json"
        with manifest_path.open("x") as output:
            json.dump(manifest, output, indent=2, sort_keys=True)
            output.write("\n")
            output.flush()
            os.fsync(output.fileno())
        os.replace(temporary, published)
        print(report_id)
        return 0
    finally:
        if temporary.exists():
            shutil.rmtree(temporary)


if __name__ == "__main__":
    raise SystemExit(main())
