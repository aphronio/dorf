"""Room-local Worker reporting protocol assets."""

from __future__ import annotations

import textwrap
from importlib.resources import files

from .job import validate_job_name

REPORT_ROOT = "/run/dorf/outbox"
CONTEXT_ROOT = "/run/dorf/context"

REPORTING_DEVELOPER_INSTRUCTIONS = textwrap.dedent(
    """\
    Dorf runtime capabilities are available in this Room. Approved Job context is under
    /run/dorf/context. At meaningful milestones, use `dorf-report progress --summary ...`.
    Record consequential defaults with `dorf-report assumption --summary ...`, and publish
    useful files with `dorf-report evidence --summary ... --file PATH`. Report only meaningful
    changes; do not emit periodic heartbeats. These reports are Worker claims until independently
    verified. Do not attempt to write to the external Job directory.
    """
).strip()


def job_context_root(job_name: str, version: int) -> str:
    """Return the detached Room context path for one Job goal version."""
    validate_job_name(job_name)
    if version < 1:
        raise ValueError("Job context version must be positive")
    return f"/run/dorf/jobs/{job_name}/context/{version}"


def job_report_root(job_name: str) -> str:
    """Return the Room-local report spool for one Job."""
    validate_job_name(job_name)
    return f"/run/dorf/jobs/{job_name}/outbox"


def assignment_reporting_instructions(job_name: str, assignment_id: str, goal_version: int) -> str:
    """Describe report capability without adding task-specific behavior."""
    context = job_context_root(job_name, goal_version)
    return textwrap.dedent(
        f"""\
        Dorf runtime capabilities are available for Job {job_name}, Assignment
        {assignment_id}. Approved Job context is under {context}. At meaningful milestones, use
        `dorf-report progress --summary ...`. Record consequential defaults with
        `dorf-report assumption --summary ...`, and publish useful files with
        `dorf-report evidence --summary ... --file PATH`. Report only meaningful changes; do
        not emit periodic heartbeats. These reports are Worker claims until independently verified.
        Do not attempt to write to the external Job directory.
        """
    ).strip()


def report_command_source() -> str:
    """Load the standalone command installed into a Room."""
    return files("dorf").joinpath("room_report.py").read_text(encoding="utf-8")
