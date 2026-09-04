# D072: Workflow deliverables are first-class Artifacts

- **Applicability:** historical
- **Areas:** workflows, persistence, client-api
- **Read when:** Reviewing the former durable Artifact model for workflow deliverables.
- **Decision history:** Superseded by D089 — 2026-08-22
- **Decision:** Add Artifact as the durable, immutable, named workflow-deliverable primitive. A
  workflow-specific typed result points to Artifact IDs; clients list Artifacts by Job and retrieve
  exact bytes by Artifact ID. Artifact metadata lives in PostgreSQL and its bytes share one neutral
  deployment-owned content-addressed blob store with Evidence. Sandbox cleanup does not remove
  Artifacts.
- **First slice, refined by D075:** `codebase-investigation` records numbered Markdown draft
  Artifacts instead of misclassifying agent prose as Evidence. A client decides how to consume a
  draft and when to request cleanup. `dorf artifact list JOB_ID` discovers
  deliverables and `dorf artifact get ARTIFACT_ID` writes exact bytes. Inspection links to retrieval
  but does not inline potentially large or binary content.
- **Boundary:** Artifacts are results or claims, not proof of their own correctness. Evidence remains
  immutable observed proof linked to the fact it supports. There is no generic result JSON bag,
  artifact publication policy, retention service, archive format, streaming API, or interaction-layer
  destination in this slice.
- **Why:** Live investigation dogfood produced a useful durable report but required extracting a
  workflow-specific JSON field. A named retrievable deliverable is the repeated client need, while
  “result” remains workflow-specific semantic meaning and Evidence has a stricter proof role.
- **Reconsider when:** Large or streaming deliverables exceed the bounded blob API, external object
  storage becomes deployment authority, or multiple producers require a richer typed ownership
  relation than the current AgentRun-produced Artifact.
