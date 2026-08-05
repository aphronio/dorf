from __future__ import annotations

import shlex
import subprocess
import textwrap
import urllib.parse
from dataclasses import dataclass

from dorf.runtime import JobBinding


@dataclass(frozen=True)
class GitAuthorIdentity:
    name: str
    email: str


def prepare_git_workspace(
    environment,
    binding: JobBinding,
    *,
    repo_full_name: str,
    token: str,
    branch: str,
    git_author: GitAuthorIdentity,
) -> None:
    """Clone one coding repository into the Assignment's fresh Job workspace."""
    repo_url = f"https://github.com/{repo_full_name}.git"
    workspace = binding.workspace
    clone = environment.execute(
        ["bash", "-lc", git_clone_workspace_script(repo_url, branch, workspace)],
        cwd="/",
        input=f"{token}\n",
    )
    if clone.returncode != 0:
        raise RuntimeError(_command_message(clone))

    for key, value in (("user.name", git_author.name), ("user.email", git_author.email)):
        result = environment.execute(
            ["git", "config", "--local", key, value],
            cwd=workspace,
        )
        if result.returncode != 0:
            raise RuntimeError(_command_message(result))

    auth = environment.execute(
        ["git", "ls-remote", "--heads", repo_url, branch],
        cwd=workspace,
        env={"GIT_TERMINAL_PROMPT": "0"},
    )
    if auth.returncode != 0:
        raise RuntimeError(
            "Job Git credentials are not configured for normal Git commands: "
            f"{_command_message(auth)}"
        )


def reset_git_workspace(environment, binding: JobBinding) -> None:
    """Recreate only the failed coding clone, preserving Assignment runtime scope."""
    result = environment.execute(
        [
            "bash",
            "-lc",
            f"rm -rf -- {shlex.quote(binding.workspace)} && "
            f"mkdir -- {shlex.quote(binding.workspace)}",
        ],
        cwd="/",
    )
    if result.returncode != 0:
        raise RuntimeError(_command_message(result))


def install_git_credentials(
    environment,
    binding: JobBinding,
    *,
    token: str,
) -> None:
    """Install a scoped Git credential through the assigned Room seam."""
    workspace = binding.workspace
    script = textwrap.dedent(
        f"""
        set -eu
        IFS= read -r GITHUB_TOKEN
        {git_credentials_store_script(workspace=workspace)}
        """
    ).strip()
    result = environment.execute(
        ["bash", "-lc", script],
        cwd=workspace,
        input=f"{token}\n",
    )
    if result.returncode != 0:
        message = _redact_secret(_command_message(result), token)
        raise RuntimeError(f"could not install Job Git credentials: {message}")


def _command_message(result: subprocess.CompletedProcess[str]) -> str:
    return result.stderr.strip() or result.stdout.strip() or "command failed"


def _redact_secret(message: str, secret: str) -> str:
    encoded_secret = urllib.parse.quote(secret, safe="")
    return message.replace(secret, "<redacted>").replace(encoded_secret, "<redacted>")


def coding_job_goal(*, job_name: str, task: str, job_branch: str, workspace: str) -> str:
    return textwrap.dedent(
        f"""
        Implement this coding task as Dorf Job {job_name}.

        Task:
        {task}

        Working rules:
        - Work only in {workspace}.
        - Keep the change scoped to this task.
        - Follow the repository instructions.
        - Use targeted repo commands while developing as needed.
        - Before finalizing, verify the work with the commands you judge relevant.
        - Commit the final implementation on {job_branch}.
        - Push HEAD to origin {job_branch}.

        Final response:
        - status: done
        - commit: <HEAD sha>
        - pr_title: <draft PR title>
        - pr_body: <draft PR body>
        - verification: <commands run and results>
        - notes: <risks, skipped checks, or empty>
        """
    ).strip()


def git_clone_workspace_script(repo_url: str, branch: str, workspace: str) -> str:
    credentials_script = textwrap.indent(
        git_credentials_store_script(workspace=workspace),
        "        ",
    )
    return textwrap.dedent(
        f"""
        set -euo pipefail
        test -d {shlex.quote(workspace)}
        test -z "$(find {shlex.quote(workspace)} -mindepth 1 -maxdepth 1 -print -quit)"
        IFS= read -r GITHUB_TOKEN
        token_file="$(mktemp)"
        helper="$(mktemp)"
        trap 'rm -f "$token_file" "$helper"' EXIT
        printf '%s\\n' "$GITHUB_TOKEN" > "$token_file"
        chmod 600 "$token_file"
        cat > "$helper" <<EOF
        #!/bin/sh
        case "\\$1" in
          *Username*) printf '%s\\n' x-access-token ;;
          *Password*) cat "$token_file" ;;
          *) printf '\\n' ;;
        esac
        EOF
        chmod 700 "$helper"
        GIT_ASKPASS="$helper" GIT_TERMINAL_PROMPT=0 git clone \\
          --branch {shlex.quote(branch)} \\
          --single-branch \\
          {shlex.quote(repo_url)} \\
          {shlex.quote(workspace)}
{credentials_script}
        git -C {shlex.quote(workspace)} ls-remote --heads \\
          {shlex.quote(repo_url)} \\
          {shlex.quote(branch)} >/dev/null
        """
    ).strip()


def git_credentials_store_script(*, workspace: str) -> str:
    return textwrap.dedent(
        f"""
        credentials_file="$HOME/.dorf-git-credentials"
        printf 'https://x-access-token:%s@github.com\\n' "$GITHUB_TOKEN" > "$credentials_file"
        chmod 600 "$credentials_file"
        git -C {shlex.quote(workspace)} config credential.helper "store --file=$credentials_file"
        git -C {shlex.quote(workspace)} config credential.useHttpPath false
        """
    ).strip()
