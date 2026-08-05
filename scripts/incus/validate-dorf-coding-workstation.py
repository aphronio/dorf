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

from dorf.adapters.agents.codex import CodexDriver
from dorf.adapters.environments import IncusConfig, incus_bridge_ipv4
from dorf.codex_room import new_codex_room_environment
from dorf.command_runner import shell_command
from dorf.provider_gateway import ProviderGateway
from dorf.repo_contract import load_repo_contract
from dorf.runtime import JobRuntime, NewJob, NewWorker, WorkerRuntime
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
        with ProviderGateway.open(bind_address=incus_bridge_ipv4(config.network)) as gateway:
            environment = new_codex_room_environment(
                config,
                args.provider_connection,
                gateway=gateway,
            )
            driver = CodexDriver(environment)
            binding = None
            worker_binding = None
            try:
                worker_binding = WorkerRuntime(store, environment, driver).spawn(
                    NewWorker(
                        worker_name,
                        provenance="coding-workflow",
                        lifecycle_policy="dedicated",
                    )
                )
                binding = JobRuntime(store, environment, driver).assign(
                    NewJob(
                        job_name,
                        worker_name,
                        (
                            f"In {IMPLEMENTATION_ARTIFACT}, write exactly: "
                            f"{IMPLEMENTATION_CONTENT.strip()}"
                        ),
                        "gpt-5.6-sol",
                        "low",
                    )
                )
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
                    ("uv", ["uv", "--version"]),
                ):
                    observed = environment.execute(binding, argv, cwd=binding.workspace)
                    _require_success(f"observe {name}", observed)
                    observed_tools[name] = observed.stdout.strip()
                evidence["tools"] = observed_tools
                clone = environment.execute(
                    binding,
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
                checkout = environment.execute(
                    binding,
                    ["git", "checkout", "--detach", args.source_commit],
                    cwd=binding.workspace,
                )
                _require_success("checkout", checkout)

                preparation_started = monotonic()
                prepare_run = prepare_coding_repository(
                    store, environment, store.get_coding_job(job_name), binding, contract
                )
                if prepare_run is None or prepare_run.exit_code != 0:
                    raise RuntimeError("repository preparation did not succeed")
                evidence["preparation_elapsed_seconds"] = round(
                    monotonic() - preparation_started, 3
                )

                first_input = store.list_job_inputs(job_name)[0]
                implementation = JobRuntime(store, environment, driver).deliver_input(
                    job_name, first_input.id
                )
                if implementation.exit_code != 0:
                    raise RuntimeError("real Codex implementation turn failed")
                artifact = environment.execute(
                    binding,
                    ["cat", IMPLEMENTATION_ARTIFACT],
                    cwd=binding.workspace,
                )
                if artifact.returncode != 0 or artifact.stdout != IMPLEMENTATION_CONTENT:
                    raise RuntimeError("Codex implementation did not create the expected artifact")
                evidence["implementation_turn"] = "succeeded"

                check_run = run_coding_job_command(
                    store,
                    environment,
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
                    environment,
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
                    job_runtime = JobRuntime(store, environment, driver)
                    current = store.get_job(job_name)
                    if current is not None and current.status != "ended":
                        try:
                            job_runtime.end(job_name, interrupt=True)
                        except Exception as error:
                            cleanup_failures.append(f"Job cleanup: {error}")
                current_worker = store.get_worker(worker_name)
                if current_worker is not None and current_worker.status != "ended":
                    try:
                        WorkerRuntime(store, environment, driver).end(
                            worker_name, interrupt=True
                        )
                    except Exception as error:
                        cleanup_failures.append(f"Worker cleanup: {error}")
                if worker_binding is not None:
                    consumer = f"room:{worker_binding.room.id}"
                    remaining_route = gateway.route_for_consumer(consumer)
                    if remaining_route is not None:
                        try:
                            gateway.revoke_route(remaining_route.id)
                        except Exception as error:
                            cleanup_failures.append(f"Provider Route cleanup: {error}")
                    room_info = subprocess.run(
                        ["incus", "info", worker_binding.environment_id],
                        text=True,
                        capture_output=True,
                        check=False,
                    )
                    if room_info.returncode == 0:
                        forced = subprocess.run(
                            ["incus", "delete", worker_binding.environment_id, "--force"],
                            text=True,
                            capture_output=True,
                            check=False,
                        )
                        if forced.returncode != 0:
                            cleanup_failures.append("forced validation Room cleanup failed")
                    if gateway.route_for_consumer(consumer) is not None:
                        cleanup_failures.append("Room-scoped provider route survived cleanup")
                    final_room = subprocess.run(
                        ["incus", "info", worker_binding.environment_id],
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
