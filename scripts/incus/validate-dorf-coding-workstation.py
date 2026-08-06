#!/usr/bin/env python3
"""Prove a candidate image through the replacement Go Job spine."""

from __future__ import annotations

import argparse
import hashlib
import json
import os
import subprocess
import tempfile
from pathlib import Path
from time import monotonic

REPOSITORY = "https://github.com/aphronio/dorf.git"
GOAL = """\
Inspect the cloned repository without modifying it. Report its current Git revision and the
installed versions of codex, git, node, pi, and uv. Keep the response concise.
"""


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument("--image", required=True)
    parser.add_argument("--image-fingerprint", required=True)
    parser.add_argument("--provider-connection", required=True)
    parser.add_argument("--source-commit", required=True)
    parser.add_argument("--proof-id", required=True)
    parser.add_argument("--project-root", type=Path, required=True)
    parser.add_argument("--evidence-dir", type=Path, required=True)
    parser.add_argument("--network", default="incusbr0")
    parser.add_argument("--root-disk-size", default="40GiB")
    args = parser.parse_args()

    _require_exact_source_checkout(args.project_root, args.source_commit)
    if not os.environ.get("DORF_DATABASE_URL", "").strip():
        parser.error("DORF_DATABASE_URL must name the prepared local PostgreSQL deployment")

    admission_key = f"image-proof:{args.proof_id}:{args.image_fingerprint}"
    job_id = "job-" + hashlib.sha256(admission_key.encode()).hexdigest()[:20]
    started = monotonic()
    args.evidence_dir.mkdir(parents=True, exist_ok=True)
    evidence: dict[str, object] = {
        "schema_version": 2,
        "image": {"alias": args.image, "fingerprint": args.image_fingerprint},
        "repository": REPOSITORY,
        "source_commit": args.source_commit,
        "provider_connection": args.provider_connection,
        "job_id": job_id,
        "execution": "Go durable Job spine",
    }

    with tempfile.TemporaryDirectory(prefix="dorf-workstation-proof.") as directory:
        temporary = Path(directory)
        binary = temporary / "dorf"
        _run(
            ["go", "build", "-o", str(binary), "./cmd/dorf"],
            cwd=args.project_root,
        )
        environment = {
            **os.environ,
            "DORF_INCUS_IMAGE": args.image,
            "DORF_INCUS_NETWORK": args.network,
            "DORF_INCUS_DISK_SIZE": args.root_disk_size,
        }
        _run(
            [str(binary), "doctor", "--provider", args.provider_connection],
            env=environment,
        )
        goal = temporary / "goal.txt"
        goal.write_text(GOAL)
        admitted = False
        try:
            admission, admission_elapsed = _run(
                [
                    str(binary),
                    "admit",
                    "--key",
                    admission_key,
                    "--goal-file",
                    str(goal),
                    "--repo",
                    REPOSITORY,
                    "--revision",
                    args.source_commit,
                    "--branch",
                    f"dorf/image-proof-{args.proof_id}",
                    "--provider",
                    args.provider_connection,
                    "--model",
                    "gpt-5.6-sol",
                    "--reasoning",
                    "low",
                ],
                env=environment,
            )
            admitted_view = json.loads(admission.stdout)
            if admitted_view.get("job_id") != job_id:
                raise RuntimeError("Go admission returned an unexpected stable Job identity")
            admitted = True
            _, worker_elapsed = _run([str(binary), "worker", "--once"], env=environment)
            inspection, _ = _run(
                [str(binary), "inspect", "--json", job_id],
                env=environment,
            )
            observed = json.loads(inspection.stdout)
            job = observed.get("job", {})
            if job.get("native_outcome") != "completed" or not job.get("native_turn_id"):
                raise RuntimeError("candidate image did not complete one real Codex turn")
        finally:
            if admitted:
                cleanup_elapsed = _cleanup_proof(binary, job_id, environment)

    evidence.update(
        {
            "native_session": job.get("session_id"),
            "native_turn": job.get("native_turn_id"),
            "native_outcome": job.get("native_outcome"),
            "admission_elapsed_seconds": admission_elapsed,
            "worker_elapsed_seconds": worker_elapsed,
            "cleanup_elapsed_seconds": cleanup_elapsed,
            "cleanup_state": "complete",
            "elapsed_seconds": round(monotonic() - started, 3),
        }
    )
    terminal_path = args.evidence_dir / "terminal.json"
    terminal_path.write_text(json.dumps(evidence, indent=2, sort_keys=True) + "\n")
    print(json.dumps({**evidence, "terminal": _artifact_record(terminal_path)}, sort_keys=True))


def _cleanup_proof(binary: Path, job_id: str, environment: dict[str, str]) -> float:
    started = monotonic()
    try:
        _run([str(binary), "cleanup", job_id], env=environment)
        _run([str(binary), "worker", "--once"], env=environment)
        _require_cleanup_complete(binary, job_id, environment)
    except RuntimeError as durable_error:
        # The worker command has returned, so the Job fence can safely cancel a
        # pending retry. Go then reconciles the exact sandbox:<Job> route and
        # hash-derived Incus name synchronously before its durable task replays.
        _run(
            [str(binary), "cleanup", "--cancel-run", "--now", job_id],
            env=environment,
        )
        _run([str(binary), "worker", "--once"], env=environment)
        try:
            _require_cleanup_complete(binary, job_id, environment)
        except RuntimeError as fallback_error:
            raise RuntimeError(
                f"exact Go cleanup fallback failed after durable cleanup error: "
                f"{durable_error}; {fallback_error}"
            ) from fallback_error
    return round(monotonic() - started, 3)


def _require_cleanup_complete(binary: Path, job_id: str, environment: dict[str, str]) -> None:
    terminal, _ = _run(
        [str(binary), "inspect", "--json", job_id],
        env=environment,
    )
    terminal_view = json.loads(terminal.stdout)
    if terminal_view.get("job", {}).get("cleanup_state") != "complete":
        raise RuntimeError("candidate image Job cleanup did not reconcile")


def _run(
    argv: list[str],
    *,
    cwd: Path | None = None,
    env: dict[str, str] | None = None,
) -> tuple[subprocess.CompletedProcess[str], float]:
    started = monotonic()
    result = subprocess.run(
        argv,
        cwd=cwd,
        env=env,
        text=True,
        capture_output=True,
        check=False,
    )
    elapsed = round(monotonic() - started, 3)
    if result.returncode != 0:
        message = result.stderr.strip() or result.stdout.strip() or "command failed"
        raise RuntimeError(f"{Path(argv[0]).name} {argv[1]} failed: {message}")
    return result, elapsed


def _require_exact_source_checkout(project_root: Path, source_commit: str) -> None:
    head, _ = _run(["git", "-C", str(project_root), "rev-parse", "HEAD"])
    if head.stdout.strip() != source_commit:
        raise RuntimeError(
            "release source commit does not match the validation checkout: "
            f"expected {source_commit}, found {head.stdout.strip()}"
        )
    status, _ = _run(
        ["git", "-C", str(project_root), "status", "--porcelain", "--untracked-files=all"]
    )
    if status.stdout:
        raise RuntimeError("release source checkout has uncommitted changes")


def _artifact_record(path: Path) -> dict[str, object]:
    content = path.read_bytes()
    return {
        "name": path.name,
        "sha256": hashlib.sha256(content).hexdigest(),
        "size": len(content),
    }


if __name__ == "__main__":
    main()
