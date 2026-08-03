import json
from dataclasses import asdict

import pytest

import dorf.runtime.documents
from dorf.runtime import (
    MAX_MODEL_ARTIFACT_BYTES,
    ArtifactInput,
    JobDirectory,
    JobDocumentStore,
)


def create_job(jobs: JobDirectory) -> None:
    jobs.create_assigned(
        name="checkout-perf",
        goal="Make checkout instant",
        worker_name="coder-checkout-perf",
        room_id="room-checkout-perf",
        workspace="/workspace/jobs/checkout-perf",
        assignment_id="assignment-checkout-perf",
        assignment_generation=1,
    )


def create_other_job(jobs: JobDirectory) -> None:
    jobs.create_assigned(
        name="inventory-audit",
        goal="Audit the inventory",
        worker_name="auditor",
        room_id="room-inventory-audit",
        workspace="/workspace/jobs/inventory-audit",
        assignment_id="assignment-inventory-audit",
        assignment_generation=1,
    )


def test_job_timeline_appends_readable_provenance_in_order_and_reuses_event_identity(
    tmp_path,
) -> None:
    jobs = JobDirectory(tmp_path / "jobs")
    create_job(jobs)
    documents = JobDocumentStore(jobs)

    first, created = documents.append_event(
        "checkout-perf",
        event_id="evt-goal-v1",
        source="runtime",
        provenance="fact",
        kind="goal-assigned",
        summary="Goal version 1 assigned",
    )
    retried, retry_created = documents.append_event(
        "checkout-perf",
        event_id="evt-goal-v1",
        source="runtime",
        provenance="fact",
        kind="goal-assigned",
        summary="Goal version 1 assigned",
    )
    second, _ = documents.append_event(
        "checkout-perf",
        event_id="report-first-finding",
        source="worker",
        provenance="claim",
        kind="progress",
        summary="Found an N+1 query in cart totals",
    )

    assert created is True
    assert retry_created is False
    assert retried == first
    events = documents.list_events("checkout-perf")
    assert [(event.sequence, event.source, event.provenance) for event in events] == [
        (1, "runtime", "fact"),
        (2, "worker", "claim"),
    ]
    assert second.summary == "Found an N+1 query in cart totals"
    files = sorted((tmp_path / "jobs" / "checkout-perf" / "timeline").glob("*.json"))
    assert [path.name for path in files] == [
        "000001-evt-goal-v1.json",
        "000002-report-first-finding.json",
    ]
    payload = json.loads(files[1].read_text())
    assert payload["job"] == "checkout-perf"
    assert payload["source"] == "worker"
    assert payload["provenance"] == "claim"


def test_job_evidence_is_content_addressed_and_rejects_untrusted_links(tmp_path) -> None:
    jobs = JobDirectory(tmp_path / "jobs")
    create_job(jobs)
    documents = JobDocumentStore(jobs)
    profile = tmp_path / "profile.txt"
    profile.write_text("p95=120ms\n")

    event, _ = documents.append_event(
        "checkout-perf",
        event_id="report-profile",
        source="worker",
        provenance="claim",
        kind="evidence",
        summary="Captured checkout profile",
        artifacts=[ArtifactInput("profile.txt", profile, "text/plain")],
    )

    artifact = event.artifacts[0]
    assert artifact.name == "profile.txt"
    assert artifact.size == 10
    stored = tmp_path / "jobs" / "checkout-perf" / artifact.path
    assert stored.read_text() == "p95=120ms\n"
    assert stored.name == artifact.digest.removeprefix("sha256:")
    link = tmp_path / "profile-link"
    link.symlink_to(profile)
    with pytest.raises(ValueError, match="regular file"):
        documents.append_event(
            "checkout-perf",
            event_id="report-link",
            source="worker",
            provenance="claim",
            kind="evidence",
            summary="Unsafe evidence",
            artifacts=[ArtifactInput("link", link, "text/plain")],
        )


def test_job_artifact_manifest_and_bounded_read_are_path_free_and_job_scoped(
    tmp_path,
) -> None:
    jobs = JobDirectory(tmp_path / "jobs")
    create_job(jobs)
    create_other_job(jobs)
    documents = JobDocumentStore(jobs)
    profile = tmp_path / "profile.json"
    profile.write_text('{"p95_ms":120,"bottleneck":"cart totals"}\n')
    documents.append_event(
        "checkout-perf",
        event_id="report-profile",
        source="worker",
        provenance="claim",
        kind="evidence",
        summary="Captured checkout profile",
        related={"assignment": "assignment-checkout-perf"},
        artifacts=[ArtifactInput("profile.json", profile, "application/json")],
    )

    artifacts = documents.list_artifacts("checkout-perf", job_id=7)
    artifact = artifacts[0]
    result = documents.read_artifact(
        "checkout-perf",
        job_id=7,
        artifact_ref=artifact.ref,
    )

    assert artifact.job_id == 7
    assert artifact.job_name == "checkout-perf"
    assert artifact.event_id == "report-profile"
    assert artifact.event_sequence == 1
    assert artifact.source == "worker"
    assert artifact.provenance == "claim"
    assert artifact.assignment_id == "assignment-checkout-perf"
    assert artifact.digest.startswith("sha256:")
    assert artifact.size == profile.stat().st_size
    assert artifact.ref == documents.list_artifacts("checkout-perf", job_id=7)[0].ref
    assert "path" not in asdict(artifact)
    assert result.status == "ok"
    assert result.artifact == artifact
    assert result.content == profile.read_text()
    assert (
        documents.read_artifact(
            "inventory-audit",
            job_id=8,
            artifact_ref=artifact.ref,
        ).status
        == "cross-job"
    )
    assert (
        documents.read_artifact(
            "checkout-perf",
            job_id=7,
            artifact_ref="artifact-v1-7-" + "0" * 64,
        ).status
        == "missing"
    )


@pytest.mark.parametrize(
    ("name", "media_type", "content", "expected"),
    [
        ("archive.zip", "application/zip", b"PK\x03\x04", "unsupported-media"),
        ("invalid.txt", "text/plain", b"\xff", "invalid-encoding"),
        ("invalid.json", "application/json", b"{not-json}", "invalid-json"),
    ],
)
def test_job_artifact_read_returns_typed_safe_content_outcomes(
    tmp_path,
    name,
    media_type,
    content,
    expected,
) -> None:
    jobs = JobDirectory(tmp_path / "jobs")
    create_job(jobs)
    documents = JobDocumentStore(jobs)
    source = tmp_path / name
    source.write_bytes(content)
    documents.append_event(
        "checkout-perf",
        event_id="report-content",
        source="worker",
        provenance="claim",
        kind="evidence",
        summary="Reported content",
        artifacts=[ArtifactInput(name, source, media_type)],
    )
    artifact = documents.list_artifacts("checkout-perf", job_id=1)[0]

    result = documents.read_artifact(
        "checkout-perf",
        job_id=1,
        artifact_ref=artifact.ref,
    )

    assert result.status == expected
    assert result.artifact == artifact
    assert result.content is None


def test_job_artifact_read_rejects_oversized_or_digest_mismatched_content(
    tmp_path,
) -> None:
    jobs = JobDirectory(tmp_path / "jobs")
    create_job(jobs)
    documents = JobDocumentStore(jobs)
    source = tmp_path / "result.txt"
    source.write_bytes(b"x" * (MAX_MODEL_ARTIFACT_BYTES + 1))
    event, _ = documents.append_event(
        "checkout-perf",
        event_id="report-result",
        source="worker",
        provenance="claim",
        kind="evidence",
        summary="Reported result",
        artifacts=[ArtifactInput("result.txt", source, "text/plain")],
    )
    artifact = documents.list_artifacts("checkout-perf", job_id=1)[0]

    assert (
        documents.read_artifact(
            "checkout-perf",
            job_id=1,
            artifact_ref=artifact.ref,
        ).status
        == "oversized"
    )

    retained = jobs.path("checkout-perf") / event.artifacts[0].path
    retained.write_bytes(b"y" * retained.stat().st_size)
    assert (
        documents.read_artifact(
            "checkout-perf",
            job_id=1,
            artifact_ref=artifact.ref,
            max_bytes=MAX_MODEL_ARTIFACT_BYTES,
        ).status
        == "oversized"
    )

    small = tmp_path / "small.txt"
    small.write_text("custody")
    small_event, _ = documents.append_event(
        "checkout-perf",
        event_id="report-small",
        source="worker",
        provenance="claim",
        kind="evidence",
        summary="Reported small result",
        artifacts=[ArtifactInput("small.txt", small, "text/plain")],
    )
    small_artifact = documents.list_artifacts("checkout-perf", job_id=1)[1]
    small_retained = jobs.path("checkout-perf") / small_event.artifacts[0].path
    small_retained.write_text("tampered")
    assert (
        documents.read_artifact(
            "checkout-perf",
            job_id=1,
            artifact_ref=small_artifact.ref,
        ).status
        == "corrupt"
    )


def test_job_evidence_enforces_streamed_file_and_job_quotas(tmp_path, monkeypatch) -> None:
    jobs = JobDirectory(tmp_path / "jobs")
    create_job(jobs)
    documents = JobDocumentStore(jobs)
    first = tmp_path / "first.bin"
    first.write_bytes(b"1234")
    monkeypatch.setattr(dorf.runtime.documents, "MAX_ARTIFACT_BYTES", 3)

    with pytest.raises(ValueError, match="exceeds 100 MiB"):
        documents.append_event(
            "checkout-perf",
            event_id="report-too-large",
            source="worker",
            provenance="claim",
            kind="evidence",
            summary="Oversized artifact",
            artifacts=[ArtifactInput("first.bin", first)],
        )

    monkeypatch.setattr(dorf.runtime.documents, "MAX_ARTIFACT_BYTES", 10)
    monkeypatch.setattr(dorf.runtime.documents, "MAX_JOB_ARTIFACT_BYTES", 5)
    documents.append_event(
        "checkout-perf",
        event_id="report-first",
        source="worker",
        provenance="claim",
        kind="evidence",
        summary="First artifact",
        artifacts=[ArtifactInput("first.bin", first)],
    )
    second = tmp_path / "second.bin"
    second.write_bytes(b"5678")
    with pytest.raises(ValueError, match="exceed 500 MiB"):
        documents.append_event(
            "checkout-perf",
            event_id="report-job-full",
            source="worker",
            provenance="claim",
            kind="evidence",
            summary="Second artifact",
            artifacts=[ArtifactInput("second.bin", second)],
        )
