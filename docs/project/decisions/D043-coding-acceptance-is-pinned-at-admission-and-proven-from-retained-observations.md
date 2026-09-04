# D043: Coding acceptance is pinned at admission and proven from retained observations

- **Applicability:** partial
- **Areas:** workflows, persistence, github
- **Read when:** Changing how coding acceptance is pinned or proven from retained observations.
- **Decision history:** Evidence authority retained by D047; Python AFK checklist/dossier mechanics
  superseded — 2026-08-08
- **Decision:** Compile the pinned issue acceptance criteria plus configured repository check and
  smoke obligations into a small workflow-owned checklist when a
  coding Job is reserved. The checklist remains explicitly human-correctable as a draft until the
  first verification attempt freezes it as the completion contract. Compute proven/unproven state
  from successful workflow-observed command records whose before/after Git
  commit is the exact dossier commit; Worker reports remain claims and never prove an item. Project
  those records, Runtime identity and Room image metadata, content-addressed Job artifacts,
  assumptions, risks, and cleanup state into one compact Markdown/JSON dossier. Publish that
  dossier to the PR as a view while the Job, workflow command records, and Job documents remain the
  authorities and audit layer. When a generic repository supplies no observable check, smoke, or
  verifier, do not manufacture a manual gate that the workflow has no mechanism to satisfy;
  preserve the existing PR-proposal path and leave final acceptance to the human GitHub boundary.
- **Why:** Reviewers need one ordered, commit-pinned assessment rather than reconstructing readiness
  through SQLite, Job-document, and artifact directories. Deriving proof from existing immutable
  facts preserves D024's provenance boundary and D026's workflow ownership without creating a
  competing evidence store or leaking coding semantics into the runtime.
- **Compatibility:** The checklist table, item verifier vocabulary, Markdown layout, JSON shape, and
  CLI names are pre-release workflow surfaces. Historical command records remain visible but cannot
  satisfy a new commit. Reports without an exact commit or turn association are displayed as
  unpinned claims and unresolved risk, never silently upgraded to fact.
- **Reconsider when:** A repository contract supplies a stronger machine-readable mapping from
  individual issue criteria to commands, a real workflow needs human attestation as durable proof,
  or repeated dossiers show that the compact selection hides evidence reviewers routinely need.
