#!/usr/bin/env python3
"""Prove the official image through Dorf's complete coding-workstation path."""

from __future__ import annotations

import argparse
import hashlib
import json
import shlex
import subprocess
import tempfile
from pathlib import Path
from time import monotonic

from dorf.command_runner import shell_command
from dorf.repo_contract import load_repo_contract
from dorf.runtime import WorkerBinding
from dorf.sdk import Dorf, IncusConfig
from dorf.workflows import CodingStore, prepare_coding_repository, run_coding_job_command

REPOSITORY = "https://github.com/aphronio/dorf.git"
IMPLEMENTATION_ARTIFACT = "dorf-workstation-proof.txt"
IMPLEMENTATION_CONTENT = "official dorf-codex workstation proof\n"


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
    contract = load_repo_contract(args.project_root)
    prepare_command = contract.commands.get("prepare")
    check_command = contract.commands.get("check")
    review = contract.review
    reviewer = None if review is None else review.agents.get("codex")
    if prepare_command is None or check_command is None or reviewer is None:
        parser.error("Dorf must declare prepare, check, and codex review contracts")

    job_name = f"workstation-proof-{args.proof_id}"
    worker_name = f"coder-{job_name}"
    config = IncusConfig(args.image, args.network, args.root_disk_size)
    started = monotonic()
    evidence: dict[str, object] = {
        "schema_version": 1,
        "image": {"alias": args.image, "fingerprint": args.image_fingerprint},
        "repository": REPOSITORY,
        "source_commit": args.source_commit,
        "provider_connection": args.provider_connection,
        "reviewer": {"name": "codex", "provider_route": "explicit"},
        "commands": {"prepare": prepare_command, "check": check_command},
        "cleanup_policy": (
            "remove Room, workspace, scoped route, and runtime state; "
            "retain redacted evidence"
        ),
    }
    args.evidence_dir.mkdir(parents=True, exist_ok=True)

    with tempfile.TemporaryDirectory(prefix="dorf-workstation-proof.") as directory:
        store = CodingStore.open(Path(directory) / "runtime.db")
        with Dorf.open_provider_gateway(config) as gateway:
            dorf = Dorf(
                store,
                environment_config=config,
                provider_connection=args.provider_connection,
                provider_gateway=gateway,
            )
            binding = None
            worker_binding = None
            try:
                worker_binding = dorf.spawn_worker(
                    worker_name,
                    provenance="coding-workflow",
                    lifecycle_policy="dedicated",
                )
                assignment = dorf.assign_job(
                    job_name,
                    worker_name=worker_name,
                    goal=(
                        f"In {IMPLEMENTATION_ARTIFACT}, write exactly: "
                        f"{IMPLEMENTATION_CONTENT.strip()}"
                    ),
                    model="gpt-5.6-sol",
                    reasoning_effort="low",
                    activate=False,
                )
                binding = assignment.binding
                execution = dorf.job_execution(job_name)
                store.create_coding_job(
                    job_name=job_name,
                    status="active",
                    metadata={
                        "task": "Prove the official coding workstation",
                        "target_repo": str(args.project_root),
                        "target_branch": "main",
                        "target_start_sha": args.source_commit,
                        "job_branch": f"dorf/{job_name}",
                    },
                )
                observed_tools = {}
                for name, argv in (
                    ("codex", ["codex", "--version"]),
                    ("git", ["git", "--version"]),
                    ("node", ["node", "--version"]),
                    ("pi", ["pi", "--version"]),
                    ("uv", ["uv", "--version"]),
                ):
                    observed = execution.execute(argv, cwd=binding.workspace)
                    _require_success(f"observe {name}", observed)
                    observed_tools[name] = observed.stdout.strip()
                evidence["tools"] = observed_tools
                clone = execution.execute(
                    [
                        "git",
                        "clone",
                        "--no-checkout",
                        REPOSITORY,
                        binding.workspace,
                    ],
                    cwd="/",
                    timeout_seconds=300,
                )
                _require_success("clone", clone)
                checkout = execution.execute(
                    ["git", "checkout", "--detach", args.source_commit],
                    cwd=binding.workspace,
                )
                _require_success("checkout", checkout)

                preparation_started = monotonic()
                prepare_run = prepare_coding_repository(
                    store, execution, store.get_coding_job(job_name), binding, contract
                )
                if prepare_run is None or prepare_run.exit_code != 0:
                    raise RuntimeError("repository preparation did not succeed")
                evidence["preparation_elapsed_seconds"] = round(
                    monotonic() - preparation_started, 3
                )

                binding = dorf.activate_job(job_name).binding
                execution = dorf.job_execution(job_name)
                first_input = store.list_job_inputs(job_name)[0]
                implementation = execution.deliver_input(first_input.id)
                if implementation.exit_code != 0:
                    raise RuntimeError("real Codex implementation turn failed")
                artifact = execution.execute(
                    ["cat", IMPLEMENTATION_ARTIFACT],
                    cwd=binding.workspace,
                )
                if artifact.returncode != 0 or artifact.stdout != IMPLEMENTATION_CONTENT:
                    raise RuntimeError("Codex implementation did not create the expected artifact")
                evidence["implementation_turn"] = "succeeded"

                check_run = run_coding_job_command(
                    store,
                    execution,
                    store.get_coding_job(job_name),
                    binding,
                    contract,
                    shell_command("check", check_command),
                )
                if check_run.exit_code != 0:
                    raise RuntimeError("repository checks failed")

                review_command = reviewer.command.replace(
                    "{dorf_review_prompt}",
                    shlex.quote(
                        "Review the current working tree. Return actionable findings, or "
                        "DORF_REVIEW_NO_FINDINGS when there are none."
                    ),
                )
                review_run = run_coding_job_command(
                    store,
                    execution,
                    store.get_coding_job(job_name),
                    binding,
                    contract,
                    shell_command(
                        "review:codex",
                        review_command,
                        timeout_seconds=review.timeout_seconds,
                        requires_provider_route=True,
                    ),
                )
                if review_run.exit_code != 0:
                    raise RuntimeError("real Codex review turn failed")
                route = gateway.route_for_consumer(f"room:{binding.room.id}")
                if route is None:
                    raise RuntimeError("review did not use an active Room-scoped route")
                evidence["reviewer"] = {
                    "name": "codex",
                    "command": reviewer.command,
                    "provider_route": "explicit",
                    "route_id": route.id,
                    "status": "succeeded",
                }

                artifacts = {}
                for run, name in (
                    (prepare_run, "prepare.log"),
                    (check_run, "check.log"),
                    (review_run, "review.log"),
                ):
                    destination = args.evidence_dir / name
                    _copy_redacted(Path(run.output_path), destination, secrets=(route.api_key,))
                    artifacts[name] = _artifact_record(destination)
                evidence["artifacts"] = artifacts
            finally:
                cleanup_failures = []
                if binding is not None:
                    current = store.get_job(job_name)
                    if current is not None and current.status != "ended":
                        try:
                            dorf.end_job(job_name, interrupt=True)
                        except Exception as error:
                            cleanup_failures.append(f"Job cleanup: {error}")
                current_worker = store.get_worker(worker_name)
                if current_worker is not None and current_worker.status != "ended":
                    try:
                        dorf.end_worker(worker_name, interrupt=True)
                    except Exception as error:
                        cleanup_failures.append(f"Worker cleanup: {error}")
                cleanup_binding = worker_binding or _recorded_worker_binding(store, worker_name)
                if cleanup_binding is not None:
                    consumer = f"room:{cleanup_binding.room.id}"
                    remaining_route = gateway.route_for_consumer(consumer)
                    if remaining_route is not None:
                        try:
                            gateway.revoke_route(remaining_route.id)
                        except Exception as error:
                            cleanup_failures.append(f"Provider Route cleanup: {error}")
                    room_info = subprocess.run(
                        ["incus", "info", cleanup_binding.environment_id],
                        text=True,
                        capture_output=True,
                        check=False,
                    )
                    if room_info.returncode == 0:
                        forced = subprocess.run(
                            ["incus", "delete", cleanup_binding.environment_id, "--force"],
                            text=True,
                            capture_output=True,
                            check=False,
                        )
                        if forced.returncode != 0:
                            cleanup_failures.append("forced validation Room cleanup failed")
                    if gateway.route_for_consumer(consumer) is not None:
                        cleanup_failures.append("Room-scoped provider route survived cleanup")
                    final_room = subprocess.run(
                        ["incus", "info", cleanup_binding.environment_id],
                        text=True,
                        capture_output=True,
                        check=False,
                    )
                    if final_room.returncode == 0:
                        cleanup_failures.append("validation Room survived cleanup")
                store.close()
                if cleanup_failures:
                    raise RuntimeError("; ".join(cleanup_failures))

    evidence["elapsed_seconds"] = round(monotonic() - started, 3)
    terminal_path = args.evidence_dir / "terminal.json"
    terminal_path.write_text(json.dumps(evidence, indent=2, sort_keys=True) + "\n")
    print(json.dumps({**evidence, "terminal": _artifact_record(terminal_path)}, sort_keys=True))


def _require_success(label: str, result: subprocess.CompletedProcess[str]) -> None:
    if result.returncode != 0:
        message = result.stderr.strip() or result.stdout.strip() or "command failed"
        raise RuntimeError(f"{label} failed: {message}")


def _require_exact_source_checkout(project_root: Path, source_commit: str) -> None:
    head = subprocess.run(
        ["git", "-C", str(project_root), "rev-parse", "HEAD"],
        text=True,
        capture_output=True,
        check=False,
    )
    _require_success("resolve release source checkout", head)
    if head.stdout.strip() != source_commit:
        raise RuntimeError(
            "release source commit does not match the validation checkout: "
            f"expected {source_commit}, found {head.stdout.strip()}"
        )
    status = subprocess.run(
        ["git", "-C", str(project_root), "status", "--porcelain", "--untracked-files=all"],
        text=True,
        capture_output=True,
        check=False,
    )
    _require_success("inspect release source checkout", status)
    if status.stdout:
        raise RuntimeError("release source checkout has uncommitted changes")


def _recorded_worker_binding(store: CodingStore, worker_name: str) -> WorkerBinding | None:
    binding = store.get_worker_binding(worker_name)
    if binding is not None:
        return binding
    worker = store.get_worker(worker_name)
    room = store.get_latest_room(worker_name)
    if worker is None or room is None:
        return None
    return WorkerBinding(worker, room)


def _artifact_record(path: Path) -> dict[str, object]:
    content = path.read_bytes()
    return {
        "name": path.name,
        "sha256": hashlib.sha256(content).hexdigest(),
        "size": len(content),
    }


def _copy_redacted(source: Path, destination: Path, *, secrets: tuple[str, ...]) -> None:
    content = source.read_text()
    for secret in secrets:
        if secret:
            content = content.replace(secret, "<redacted>")
    destination.write_text(content)


if __name__ == "__main__":
    main()
