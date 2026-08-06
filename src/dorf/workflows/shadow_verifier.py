"""One concrete, disposable Pi + DeepSeek shadow review."""

from __future__ import annotations

import json
import os
import secrets
import shlex
from pathlib import Path
from typing import Protocol

from dorf.command_runner import CommandSpec, run_job_command
from dorf.github_app import GitHubRepositoryClient
from dorf.provider_gateway import ProviderGateway
from dorf.workflows.coding_store import CodingJob, CodingStore

CLONE = "/workspace/verifier"
NO_FINDINGS = "DORF_REVIEW_NO_FINDINGS"
MODEL = "deepseek-v4-flash"


class ReviewSandboxController(Protocol):
    """Retained collaborator seam for the later independent-review slice."""

    def spawn_worker(self, name: str, **kwargs): ...

    def worker_execution(self, name: str): ...

    def end_worker(self, name: str, *, interrupt: bool = False): ...


def deepseek_extension(base_url: str) -> str:
    return f"""export default function (pi) {{
  pi.registerProvider("dorf-deepseek", {{
    baseUrl: {json.dumps(base_url)}, apiKey: "${{DORF_PROVIDER_ROUTE_KEY}}",
    api: "openai-responses", models: [{{ id: "deepseek/{MODEL}",
    name: "DeepSeek V4 Flash", reasoning: true, input: ["text"],
    cost: {{input:0,output:0,cacheRead:0,cacheWrite:0}},
    contextWindow: 1000000, maxTokens: 384000,
    thinkingLevelMap: {{low:null,medium:null,high:"default",max:"max"}} }}]
  }});
}}"""


def deepseek_command() -> str:
    return (
        f"pi -p --provider dorf-deepseek "
        f"--model dorf-deepseek/deepseek/{MODEL} "
        "--thinking max --tools read,grep,find,ls "
        "--no-session --no-approve --no-context-files --no-extensions "
        "--no-skills --no-prompt-templates "
        "-e /tmp/dorf-provider.mjs @/tmp/dorf-protocol.md"
    )


def run_shadow_review(
    store: CodingStore,
    job: CodingJob,
    dorf: ReviewSandboxController,
    gateway: ProviderGateway,
    github: GitHubRepositoryClient,
    token: str,
    *,
    commit: str | None = None,
):
    """Review one remote Job head and always retire the verifier Room."""
    repo = job.metadata["github_repo"]
    remote_commit = github.get_branch_sha(repo, job.job_branch)
    if commit is not None and remote_commit != commit:
        raise RuntimeError("Verifier commit does not match the remote Job branch")
    commit = commit or remote_commit
    worker = f"verify-{job.job_name[:32]}-{secrets.token_hex(3)}"
    binding = dorf.spawn_worker(
        worker, provenance="verification", lifecycle_policy="dedicated"
    )
    try:
        execution = dorf.worker_execution(worker)
        route = gateway.route_for_consumer(f"room:{binding.room.id}")
        if route is None:
            raise RuntimeError("Verifier Room has no provider route")
        clone = f"""
set -euo pipefail
IFS= read -r token
askpass=$(mktemp)
trap 'rm -f "$askpass"; unset token' EXIT
cat >"$askpass" <<'EOF'
#!/bin/sh
case "$1" in *Username*) echo x-access-token;; *) printf '%s' "$GITHUB_TOKEN";; esac
EOF
chmod 700 "$askpass"
rm -rf {CLONE}
GITHUB_TOKEN="$token" GIT_ASKPASS="$askpass" GIT_TERMINAL_PROMPT=0 \
  git clone --quiet --single-branch --branch {shlex.quote(job.job_branch)} \
  {shlex.quote(f"https://github.com/{repo}.git")} {CLONE}
git -C {CLONE} checkout --quiet --detach {commit}
git -C {CLONE} remote set-url origin {shlex.quote(f"https://github.com/{repo}.git")}
"""
        result = execution.execute(["bash", "-lc", clone], input=f"{token}\n", timeout_seconds=300)
        if result.returncode:
            raise RuntimeError("Could not clone the verifier commit")

        extension = deepseek_extension(route.base_url)
        protocol = f"""Shadow-review the exact diff in /tmp/dorf-review.diff.
Use only read, grep, find, and ls. Never modify files.
Focus on correctness, regressions, unsafe authority, and missing tests.
If there are no findings, reply exactly: {NO_FINDINGS}
Otherwise return concise actionable findings with file and line references.
"""
        setup = f"""
set -euo pipefail
umask 077
cat >/tmp/dorf-provider.mjs
cat >/tmp/dorf-protocol.md <<'EOF'
{protocol}
EOF
git -C {CLONE} diff {shlex.quote(job.target_start_sha)}..{commit} >/tmp/dorf-review.diff
"""
        result = execution.execute(["bash", "-lc", setup], input=extension)
        if result.returncode:
            raise RuntimeError("Could not prepare the verifier protocol")

        pi = (
            f"before=$(git rev-parse HEAD); {deepseek_command()}; rc=$?; "
            "after=$(git rev-parse HEAD); dirty=$(git status --porcelain); "
            "if test \"$before\" != \"$after\" || test -n \"$dirty\"; then exit 70; fi; "
            "exit $rc"
        )
        command = execution.process_command(
            ["bash", "-lc", pi], cwd=CLONE, provider_route=True
        )
        run = run_job_command(
            store,
            job.job_name,
            Path("/"),
            CommandSpec(
                kind="verify-role:diff",
                command=command,
                preview=f"pi deepseek-v4-flash shadow diff at {commit}",
                timeout_seconds=1800,
            ),
            os.environ.copy(),
        )
        return store.set_command_run_git_commits(
            run.id, before=commit, after=commit if run.exit_code == 0 else None
        )
    finally:
        dorf.end_worker(worker, interrupt=True)
