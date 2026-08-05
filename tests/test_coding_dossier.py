import json
import subprocess
from dataclasses import asdict, replace
from pathlib import Path

import pytest
from typer.testing import CliRunner

from dorf.cli import app
from dorf.repo_contract import RepoContract, ReviewAgent, ReviewConfig
from dorf.runtime import ArtifactInput, JobRuntime, NewJob, NewWorker, WorkerRuntime
from dorf.workflows import CodingStore
from dorf.workflows.coding_dossier import (
    AcceptanceItem,
    build_proof_dossier,
    compile_acceptance_checklist,
    render_proof_dossier,
    review_output_has_no_findings,
)


class ReadyEnvironment:
    environment_type = "incus-vm"
    workspace = "/workspace"

    def environment_id(self, worker_name):
        return f"dorf-{worker_name}"

    def initial_metadata(self, worker_name):
        return {
            "image_fingerprint": "f" * 64,
            "image_release": "v0.1.2",
        }

    def create(self, binding):
        pass

    def execute(self, binding, argv, **kwargs):
        return subprocess.CompletedProcess(argv, 0, "", "")

    def stop(self, binding):
        return "stopped"

    def destroy(self, binding):
        return "deleted"


class ReadyAgent:
    agent_type = "codex"

    def prepare(self, binding):
        pass


def assigned_coding_job(tmp_path: Path):
    store = CodingStore.open(tmp_path / "state.sqlite3")
    environment = ReadyEnvironment()
    WorkerRuntime(store, environment, ReadyAgent()).spawn(NewWorker("coder-proof"))
    binding = JobRuntime(store, environment, ReadyAgent()).assign(
        NewJob("proof", "coder-proof", "Pinned goal", "gpt-5.6-sol", "high")
    )
    repo = tmp_path / "repo"
    repo.mkdir()
    job = store.create_coding_job(
        job_name="proof",
        status="active",
        metadata={
            "task": "Prove the change",
            "target_repo": str(repo),
            "target_branch": "main",
            "target_start_sha": "a" * 40,
            "job_branch": "dorf/proof",
        },
    )
    return store, job, binding


def test_acceptance_is_compiled_from_pinned_issue_and_verification_contract() -> None:
    prompt = """\
Issue body:
## Acceptance criteria
- [ ] Checkout is faster than 200ms.
- [ ] The fallback remains correct.

## Step-change bar
Proof, not vibes.
"""
    contract = RepoContract(
        mode="configured",
        commands={"prepare": "uv sync", "check": "uv run pytest", "smoke": "./smoke.sh"},
        env={},
        review=ReviewConfig(
            agents={
                "codex": ReviewAgent(
                    "codex", "codex exec {dorf_review_prompt}", enabled=True
                )
            }
        ),
    )

    pinned_review_command = "codex exec 'pinned Dorf review protocol'"
    checklist = compile_acceptance_checklist(
        prompt,
        contract,
        review_commands={"codex": pinned_review_command},
    )

    assert [item.text for item in checklist] == [
        "Checkout is faster than 200ms.",
        "The fallback remains correct.",
        "Repository check passes: uv run pytest",
        "Repository smoke passes: ./smoke.sh",
        "Independent review by codex reports no findings",
    ]
    assert [item.verifier for item in checklist] == [
        "review",
        "review",
        "command",
        "command",
        "review",
    ]
    assert all(
        pinned_review_command in item.verifier_command
        for item in checklist
        if item.verifier == "review"
    )


def test_acceptance_reviewer_keys_survive_slug_collisions(tmp_path: Path) -> None:
    contract = RepoContract(
        mode="configured",
        commands={},
        env={},
        review=ReviewConfig(
            agents={
                "foo-bar": ReviewAgent("foo-bar", "first", enabled=True),
                "foo_bar": ReviewAgent("foo_bar", "second", enabled=True),
            }
        ),
    )
    checklist = compile_acceptance_checklist(
        "Pinned goal",
        contract,
        review_commands={"foo-bar": "first", "foo_bar": "second"},
    )
    store, _, _ = assigned_coding_job(tmp_path)

    recorded = store.record_acceptance_checklist(
        "proof", goal="Pinned goal", items=checklist
    )

    reviewer_keys = [
        item.key for item in recorded.items if item.verifier_ref in {"foo-bar", "foo_bar"}
    ]
    assert len(reviewer_keys) == len(set(reviewer_keys)) == 2


def test_generic_repo_does_not_compile_an_unprovable_manual_gate() -> None:
    contract = RepoContract(mode="generic", commands={}, env={})

    checklist = compile_acceptance_checklist("Implement the requested change", contract)

    assert checklist == ()


def test_command_acceptance_requires_an_exact_pinned_command(tmp_path: Path) -> None:
    store, _, _ = assigned_coding_job(tmp_path)

    with pytest.raises(ValueError, match="pinned command"):
        store.record_acceptance_checklist(
            "proof",
            goal="Pinned goal",
            items=(AcceptanceItem("repo-check", "Checks pass", "contract", "command", "check"),),
        )


def test_acceptance_can_be_corrected_until_frozen(tmp_path: Path) -> None:
    store, _, _ = assigned_coding_job(tmp_path)
    initial = (AcceptanceItem("issue-1", "Original", "issue", "review", "*", "{}"),)
    store.record_acceptance_checklist("proof", goal="Pinned goal", items=initial)

    corrected = (replace(initial[0], text="Human-corrected"),)
    store.replace_acceptance_checklist("proof", corrected)
    assert store.get_acceptance_checklist("proof").items == corrected

    store.freeze_acceptance_checklist("proof")
    with pytest.raises(RuntimeError, match="already governs"):
        store.replace_acceptance_checklist("proof", initial)


def test_acceptance_cli_applies_human_markdown_correction(tmp_path: Path, monkeypatch) -> None:
    data_home = tmp_path / "data"
    monkeypatch.setenv("XDG_DATA_HOME", str(data_home))
    store, job, _ = assigned_coding_job(data_home / "dorf")
    Path(job.target_repo, ".dorf.toml").write_text('[commands]\ncheck = "pytest"\n')
    initial = (AcceptanceItem("goal-1", "Original", "goal", "manual", ""),)
    store.record_acceptance_checklist("proof", goal="Pinned goal", items=initial)
    correction = tmp_path / "acceptance.md"
    correction.write_text("## Acceptance criteria\n- [ ] Human-corrected behavior\n")

    result = CliRunner().invoke(
        app,
        ["acceptance", "proof", "--from-file", str(correction), "--json"],
    )

    assert result.exit_code == 0, result.output
    corrected = store.get_acceptance_checklist("proof")
    assert corrected.revision == 2
    assert corrected.items[0].source == "human"
    assert corrected.items[0].text == "Human-corrected behavior"


def test_dossier_proofs_are_commit_pinned_and_new_head_invalidates_them(tmp_path: Path) -> None:
    store, job, binding = assigned_coding_job(tmp_path)
    commit = "b" * 40
    store.record_acceptance_checklist(
        "proof",
        goal="Pinned goal",
        items=(
            AcceptanceItem(
                "repo-check",
                "Checks pass",
                "contract",
                "command",
                "check",
                "uv run pytest",
            ),
        ),
    )
    output = tmp_path / "check.log"
    output.write_text("1 passed\n")
    run = store.create_command_run("proof", "check", "uv run pytest", str(output))
    store.finish_command_run(run.id, "succeeded", 0)
    store.set_command_run_git_commits(run.id, before=commit, after=commit)
    store.documents.append_event(
        "proof",
        event_id=f"evt-command-{run.id}-output",
        source="workflow",
        provenance="fact",
        kind="command-result",
        summary="check succeeded (exit 0)",
        related={
            "assignment": binding.assignment.id,
            "run": str(run.id),
            "room": binding.room.id,
            "worker": binding.worker.name,
        },
        artifacts=[ArtifactInput("check-output.log", output, "text/plain")],
    )

    proven = build_proof_dossier(store, job, binding, commit_sha=commit)
    stale = build_proof_dossier(store, job, binding, commit_sha="c" * 40)

    assert proven.acceptance[0].status == "proven"
    assert proven.acceptance[0].evidence[0].provenance == "fact"
    assert proven.acceptance[0].evidence[0].commit_sha == commit
    assert proven.acceptance[0].evidence[0].artifacts[0].digest.startswith("sha256:")
    assert stale.acceptance[0].status == "unproven"
    assert "older commit" in stale.acceptance[0].reason


def test_ready_verdict_requires_the_exact_proof_commit(tmp_path: Path) -> None:
    store, job, binding = assigned_coding_job(tmp_path)
    verified_commit = "b" * 40
    other_commit = "c" * 40
    store.record_acceptance_checklist(
        "proof",
        goal="Pinned goal",
        items=(
            AcceptanceItem(
                "repo-check",
                "Checks pass",
                "contract",
                "command",
                "check",
                "pytest",
            ),
        ),
    )
    run = store.create_command_run(
        "proof", "check", "pytest", str(tmp_path / "check.log")
    )
    store.finish_command_run(run.id, "succeeded", 0)
    store.set_command_run_git_commits(
        run.id, before=verified_commit, after=verified_commit
    )
    store.update_status("proof", "ready")
    store.set_metadata_value("proof", "proof_commit", other_commit)
    job = store.get_coding_job("proof")
    assert job is not None

    not_ready = build_proof_dossier(
        store, job, binding, commit_sha=verified_commit
    )

    store.set_metadata_value("proof", "proof_commit", verified_commit)
    job = store.get_coding_job("proof")
    assert job is not None
    ready = build_proof_dossier(store, job, binding, commit_sha=verified_commit)

    assert not_ready.acceptance[0].status == "proven"
    assert not_ready.verdict == "not ready"
    assert any("readiness is pinned" in risk for risk in not_ready.unresolved_risks)
    assert ready.verdict == "ready"


def test_command_acceptance_rejects_a_different_command_under_the_same_name(
    tmp_path: Path,
) -> None:
    store, job, binding = assigned_coding_job(tmp_path)
    commit = "b" * 40
    store.record_acceptance_checklist(
        "proof",
        goal="Pinned goal",
        items=(
            AcceptanceItem(
                "repo-check",
                "Repository check passes: pytest",
                "contract",
                "command",
                "check",
                verifier_command="pytest",
            ),
        ),
    )
    run = store.create_command_run("proof", "check", "true", str(tmp_path / "check.log"))
    store.finish_command_run(run.id, "succeeded", 0)
    store.set_command_run_git_commits(run.id, before=commit, after=commit)

    dossier = build_proof_dossier(store, job, binding, commit_sha=commit)

    assert dossier.acceptance[0].status == "unproven"


def test_command_acceptance_uses_the_newest_observation_at_the_commit(
    tmp_path: Path,
) -> None:
    store, job, binding = assigned_coding_job(tmp_path)
    commit = "b" * 40
    store.record_acceptance_checklist(
        "proof",
        goal="Pinned goal",
        items=(
            AcceptanceItem(
                "repo-check",
                "Repository check passes: pytest",
                "contract",
                "command",
                "check",
                verifier_command="pytest",
            ),
        ),
    )
    passed = store.create_command_run(
        "proof", "check", "pytest", str(tmp_path / "passed.log")
    )
    store.finish_command_run(passed.id, "succeeded", 0)
    store.set_command_run_git_commits(passed.id, before=commit, after=commit)
    failed = store.create_command_run(
        "proof", "check", "pytest", str(tmp_path / "failed.log")
    )
    store.finish_command_run(failed.id, "failed", 1)
    store.set_command_run_git_commits(failed.id, before=commit, after=commit)

    dossier = build_proof_dossier(store, job, binding, commit_sha=commit)

    assert dossier.acceptance[0].status == "unproven"


def test_review_acceptance_rejects_findings_even_when_sentinel_is_also_present(
    tmp_path: Path,
) -> None:
    store, job, binding = assigned_coding_job(tmp_path)
    commit = "b" * 40
    store.record_acceptance_checklist(
        "proof",
        goal="Pinned goal",
        items=(
            AcceptanceItem(
                "review-codex",
                "Independent review is clean",
                "contract",
                "review",
                "codex",
                "reviewer",
            ),
        ),
    )
    output = tmp_path / "review.log"
    output.write_text("A blocking finding\nDORF_REVIEW_NO_FINDINGS\n")
    run = store.create_command_run("proof", "review:codex", "reviewer", str(output))
    store.finish_command_run(run.id, "succeeded", 0)
    store.set_command_run_git_commits(run.id, before=commit, after=commit)

    dossier = build_proof_dossier(store, job, binding, commit_sha=commit)

    assert dossier.acceptance[0].status == "unproven"


def test_codex_review_parser_accepts_real_clean_transcript_with_telemetry_echo() -> None:
    output = """\
Reading additional input from stdin...
OpenAI Codex v0.150.0
--------
workdir: /workspace/jobs/one-file
codex
DORF_REVIEW_NO_FINDINGS
tokens used
8,859
DORF_REVIEW_NO_FINDINGS
"""

    assert review_output_has_no_findings(output) is True


@pytest.mark.parametrize(
    "output",
    [
        "codex\n- [P1] A real finding\ntokens used\n8,859\n- [P1] A real finding\n",
        (
            "codex\n- [P1] A real finding\nDORF_REVIEW_NO_FINDINGS\n"
            "tokens used\n8,859\nDORF_REVIEW_NO_FINDINGS\n"
        ),
        "codex\nDORF_REVIEW_NO_FINDINGS\ntokens used\n8,859\nA trailing finding\n",
        "codex\nDORF_REVIEW_NO_FINDINGS\ntokens used\nnot-a-count\nDORF_REVIEW_NO_FINDINGS\n",
        "codex\nDORF_REVIEW_NO_FINDINGS\ntokens used\n8,59\nDORF_REVIEW_NO_FINDINGS\n",
        "codex\nDORF_REVIEW_NO_FINDINGS\ntokens used\nDORF_REVIEW_NO_FINDINGS\n",
        "preamble\nDORF_REVIEW_NO_FINDINGS\n",
        "finding\nDORF_REVIEW_NO_FINDINGS\n",
    ],
)
def test_review_parser_rejects_findings_or_malformed_harness_output(output: str) -> None:
    assert review_output_has_no_findings(output) is False


def test_review_acceptance_uses_the_newest_verdict_at_the_commit(
    tmp_path: Path,
) -> None:
    store, job, binding = assigned_coding_job(tmp_path)
    commit = "b" * 40
    store.record_acceptance_checklist(
        "proof",
        goal="Pinned goal",
        items=(
            AcceptanceItem(
                "review-codex",
                "Independent review is clean",
                "contract",
                "review",
                "codex",
                "reviewer",
            ),
        ),
    )
    clean = store.create_command_run(
        "proof", "review:codex", "reviewer", str(tmp_path / "clean.log")
    )
    store.finish_command_run(clean.id, "succeeded", 0)
    store.set_command_run_git_commits(clean.id, before=commit, after=commit)
    store.documents.append_event(
        "proof",
        event_id=f"evt-review-{clean.id}-verdict",
        source="workflow",
        provenance="fact",
        kind="review-verdict",
        summary="review:codex observed no findings",
        related={"commit": commit, "run": str(clean.id), "verdict": "no-findings"},
    )
    findings = store.create_command_run(
        "proof", "review:codex", "reviewer", str(tmp_path / "findings.log")
    )
    store.finish_command_run(findings.id, "succeeded", 0)
    store.set_command_run_git_commits(findings.id, before=commit, after=commit)
    store.documents.append_event(
        "proof",
        event_id=f"evt-review-{findings.id}-verdict",
        source="workflow",
        provenance="fact",
        kind="review-verdict",
        summary="review:codex observed findings",
        related={"commit": commit, "run": str(findings.id), "verdict": "findings"},
    )

    dossier = build_proof_dossier(store, job, binding, commit_sha=commit)

    assert dossier.acceptance[0].status == "unproven"


def test_default_dossier_omits_multiline_commands_but_json_keeps_them(
    tmp_path: Path,
) -> None:
    store, job, binding = assigned_coding_job(tmp_path)
    commit = "b" * 40
    command = "codex exec --readonly <<'REVIEW'\nreview the exact diff\nREVIEW"
    store.record_acceptance_checklist("proof", goal="Pinned goal", items=())
    run = store.create_command_run(
        "proof", "review:codex", command, str(tmp_path / "review.log")
    )
    store.finish_command_run(run.id, "succeeded", 0)
    store.set_command_run_git_commits(run.id, before=commit, after=commit)

    dossier = build_proof_dossier(store, job, binding, commit_sha=commit)
    markdown = render_proof_dossier(dossier)
    structured = json.dumps(asdict(dossier), sort_keys=True)

    assert dossier.independent_review[0].command == command
    assert json.loads(structured)["independent_review"][0]["command"] == command
    assert command not in markdown
    assert f"run:{run.id}" in markdown
    assert len(markdown.splitlines()) <= 50


def test_default_dossier_compacts_many_unproven_acceptance_items(
    tmp_path: Path,
) -> None:
    store, job, binding = assigned_coding_job(tmp_path)
    commit = "b" * 40
    texts = [
        f"Behavior {name} is preserved"
        for name in (
            "alpha",
            "bravo",
            "charlie",
            "delta",
            "echo",
            "foxtrot",
            "golf",
            "hotel",
            "india",
            "juliet",
            "kilo",
        )
    ]
    store.record_acceptance_checklist(
        "proof",
        goal="Pinned goal",
        items=tuple(
            AcceptanceItem(
                f"issue-{position}",
                text,
                "issue",
                "command",
                "check",
                "pytest",
            )
            for position, text in enumerate(texts, start=1)
        ),
    )

    dossier = build_proof_dossier(store, job, binding, commit_sha=commit)
    markdown = render_proof_dossier(dossier)
    structured = json.loads(json.dumps(asdict(dossier), sort_keys=True))

    assert dossier.unresolved_risks == (
        f"11 of 11 acceptance items remain unproven at {commit}",
    )
    for text, result in zip(texts, structured["acceptance"], strict=True):
        detailed_reason = f"No passing observed evidence at {commit}: {text}"
        assert result["reason"] == detailed_reason
        assert detailed_reason not in markdown
        assert markdown.count(text) == 1
        assert f"{text} — No passing observed evidence at {commit}" in markdown
    assert markdown.count("acceptance items remain unproven") == 1
    assert len(markdown.splitlines()) <= 57


def test_review_acceptance_rejects_a_changed_command_under_the_same_reviewer(
    tmp_path: Path,
) -> None:
    store, job, binding = assigned_coding_job(tmp_path)
    commit = "b" * 40
    store.record_acceptance_checklist(
        "proof",
        goal="Pinned goal",
        items=(
            AcceptanceItem(
                "review-codex",
                "Independent review is clean",
                "contract",
                "review",
                "codex",
                "reviewer --pinned-protocol",
            ),
        ),
    )
    run = store.create_command_run(
        "proof",
        "review:codex",
        "reviewer --changed-protocol",
        str(tmp_path / "review.log"),
    )
    store.finish_command_run(run.id, "succeeded", 0)
    store.set_command_run_git_commits(run.id, before=commit, after=commit)
    store.documents.append_event(
        "proof",
        event_id=f"evt-review-{run.id}-verdict",
        source="workflow",
        provenance="fact",
        kind="review-verdict",
        summary="review:codex observed no findings",
        related={"commit": commit, "run": str(run.id), "verdict": "no-findings"},
    )

    dossier = build_proof_dossier(store, job, binding, commit_sha=commit)

    assert dossier.acceptance[0].status == "unproven"


def test_review_acceptance_uses_immutable_observed_verdict_not_mutable_run_log(
    tmp_path: Path,
) -> None:
    store, job, binding = assigned_coding_job(tmp_path)
    commit = "b" * 40
    store.record_acceptance_checklist(
        "proof",
        goal="Pinned goal",
        items=(
            AcceptanceItem(
                "review-codex",
                "Independent review is clean",
                "contract",
                "review",
                "codex",
                "reviewer",
            ),
        ),
    )
    output = tmp_path / "review.log"
    output.write_text("DORF_REVIEW_NO_FINDINGS\n")
    run = store.create_command_run("proof", "review:codex", "reviewer", str(output))
    store.finish_command_run(run.id, "succeeded", 0)
    store.set_command_run_git_commits(run.id, before=commit, after=commit)
    store.documents.append_event(
        "proof",
        event_id=f"evt-command-{run.id}-output",
        source="workflow",
        provenance="fact",
        kind="command-result",
        summary="review:codex succeeded (exit 0)",
        related={"run": str(run.id)},
        artifacts=[ArtifactInput("review-output.log", output, "text/plain")],
    )
    store.documents.append_event(
        "proof",
        event_id=f"evt-review-{run.id}-verdict",
        source="workflow",
        provenance="fact",
        kind="review-verdict",
        summary="review:codex observed no findings",
        related={"commit": commit, "run": str(run.id), "verdict": "no-findings"},
    )
    output.write_text("A finding inserted after observation\n")

    mutated = build_proof_dossier(store, job, binding, commit_sha=commit)
    output.unlink()
    deleted = build_proof_dossier(store, job, binding, commit_sha=commit)

    assert mutated.acceptance[0].status == "proven"
    assert deleted.acceptance[0].status == "proven"
    assert mutated.acceptance[0].evidence[0].artifacts[0].name == "review-output.log"
    assert mutated.relevant_artifacts[0].name == "review-output.log"


def test_dossier_reports_missing_immutable_image_provenance_as_a_risk(tmp_path: Path) -> None:
    store, job, binding = assigned_coding_job(tmp_path)
    binding = replace(
        binding,
        room=replace(
            binding.room,
            metadata={"template": "dorf-codex", "network": "incusbr0"},
        ),
    )

    dossier = build_proof_dossier(store, job, binding, commit_sha="b" * 40)

    assert any(
        "immutable image fingerprint" in risk for risk in dossier.unresolved_risks
    )


def test_dossier_keeps_claims_separate_and_renders_ordered_compact_sections(
    tmp_path: Path,
) -> None:
    store, job, binding = assigned_coding_job(tmp_path)
    store.record_acceptance_checklist(
        "proof",
        goal="Pinned goal",
        items=(AcceptanceItem("issue-1", "Behavior is correct", "issue", "manual", ""),),
    )
    claim_artifact = tmp_path / "worker.txt"
    claim_artifact.write_text("worker says done\n")
    store.documents.append_event(
        "proof",
        event_id="report-assumption",
        source="worker",
        provenance="claim",
        kind="assumption",
        summary="Assumed the legacy format is unsupported",
        related={
            "assignment": binding.assignment.id,
            "conversation": binding.conversation.id,
            "room": binding.room.id,
            "worker": binding.worker.name,
        },
        artifacts=[ArtifactInput("worker.txt", claim_artifact, "text/plain")],
    )

    dossier = build_proof_dossier(store, job, binding, commit_sha="b" * 40)
    markdown = render_proof_dossier(dossier)

    assert dossier.assumptions_and_claims[0].provenance == "claim"
    assert dossier.assumptions_and_claims[0].commit_sha is None
    assert any("Unpinned Worker claim" in risk for risk in dossier.unresolved_risks)
    headings = [
        "## Outcome / verdict",
        "## Exact commit",
        "## Acceptance status",
        "## Environment / image provenance",
        "## Checks",
        "## Independent review",
        "## Assumptions and Worker claims",
        "## Unresolved risks",
        "## Relevant artifacts",
        "## Cleanup state",
    ]
    assert [markdown.index(heading) for heading in headings] == sorted(
        markdown.index(heading) for heading in headings
    )
    assert "[worker claim]" in markdown
    assert "artifact-v1-" in markdown
    assert "image_fingerprint" in markdown


def test_delayed_worker_claim_does_not_infer_the_later_active_turn(
    tmp_path: Path,
) -> None:
    store, job, binding = assigned_coding_job(tmp_path)
    goal_input = store.list_job_inputs(job.job_name)[0]
    first_turn, _ = store.admit_job_turn(
        goal_input, output_path=str(tmp_path / "first.log")
    )
    store.finish_job_turn(
        first_turn.id,
        status="succeeded",
        exit_code=0,
        error=None,
    )
    later_input, _ = store.enqueue_job_input(
        job.job_name,
        message_id="later-input",
        text="Continue with another turn",
    )
    later_turn, _ = store.admit_job_turn(
        later_input, output_path=str(tmp_path / "later.log")
    )
    store.documents.append_event(
        job.job_name,
        event_id="report-delayed",
        source="worker",
        provenance="claim",
        kind="progress",
        summary="Report emitted during the earlier turn",
        related={
            "assignment": binding.assignment.id,
            "conversation": binding.conversation.id,
            "room": binding.room.id,
            "worker": binding.worker.name,
        },
    )

    dossier = build_proof_dossier(store, job, binding, commit_sha="b" * 40)

    assert later_turn.status == "running"
    assert dossier.assumptions_and_claims[0].turn_id is None
    assert any(
        "without an exact Job turn association" in risk
        for risk in dossier.unresolved_risks
    )


def test_worker_claim_accepts_only_an_explicit_turn_from_its_job_conversation(
    tmp_path: Path,
) -> None:
    store, job, binding = assigned_coding_job(tmp_path)
    goal_input = store.list_job_inputs(job.job_name)[0]
    turn, _ = store.admit_job_turn(
        goal_input, output_path=str(tmp_path / "turn.log")
    )
    related = {
        "assignment": binding.assignment.id,
        "conversation": binding.conversation.id,
        "room": binding.room.id,
        "worker": binding.worker.name,
    }
    store.documents.append_event(
        job.job_name,
        event_id="report-explicit-turn",
        source="worker",
        provenance="claim",
        kind="progress",
        summary="Explicitly related claim",
        related={**related, "turn": str(turn.id)},
    )
    store.documents.append_event(
        job.job_name,
        event_id="report-invalid-turn",
        source="worker",
        provenance="claim",
        kind="progress",
        summary="Claim with an invalid turn",
        related={**related, "turn": str(turn.id + 1000)},
    )

    dossier = build_proof_dossier(store, job, binding, commit_sha="b" * 40)

    assert [claim.turn_id for claim in dossier.assumptions_and_claims] == [turn.id, None]
