# D091: Profile verification gates admission, not admitted runtime

- **Applicability:** current
- **Areas:** sandboxes, harnesses, persistence
- **Read when:** Changing profile verification, concurrent admission, or admitted runtime resolution.
- **Decision history:** Accepted profile concurrency correction — 2026-08-22
- **Decision:** A successful current verification receipt gates default selection and brand-new Job
  admission. It is not a lease held by a Job and it is not rechecked when an already-admitted Job
  resolves its pinned runtime. Any number of Jobs may concurrently use one verified profile and own
  independent Sandboxes.
- **Re-verification:** An explicit fresh verification may replace the singleton receipt while Jobs
  using the unchanged profile definition remain incomplete. Existing Jobs continue resolving that
  definition. New admission is serialized with the attempt and remains fenced until its probe and
  cleanup settle successfully. PostgreSQL grants one process the verification operation at a time;
  a concurrent invocation fails before touching the proof Sandbox, while connection loss releases
  ownership and the next invocation resumes the same durable attempt. Exact replay of an existing
  admission key remains idempotent.
- **Mutation:** Provider, image, Harness, and settings changes remain blocked while any referencing
  Job has incomplete cleanup because Jobs pin the profile name rather than an immutable definition
  revision. A changed definition clears default and verification state after that fence permits it.
- **Why:** Verification proves eligibility for future admission; it does not allocate profile
  capacity. Treating its mutable receipt as live runtime authority made a reusable profile appear
  occupied and forced duplicate profile names during a contract refresh.
- **Proof:** PostgreSQL coverage admits many Jobs through one receipt, grants one verifier ownership,
  resumes its exact tuple after interruption, serializes new admission with receipt replacement,
  preserves admitted runtime resolution and exact admission replay while a later receipt is pending
  or failed, and retains the changed-definition fence until every referencing Job completes cleanup.
