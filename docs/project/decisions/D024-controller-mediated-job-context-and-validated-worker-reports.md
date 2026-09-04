# D024: Controller-mediated Job context and validated Worker reports

- **Applicability:** historical
- **Areas:** core, workflows, persistence
- **Read when:** Changing how approved context enters sandboxes or validated Worker claims and artifacts leave them.
- **Decision history:** Superseded by the Go core in D047, Message/AgentRun boundary in D052, and derived product
  history in D061 — 2026-08-10
- **Decision:** Project approved Job context into the Room as a detached copy with no write-back path;
  never mount the external Job directory writable. Give the Worker a Room-local `dorf-report`
  command, described through harness-level runtime capability guidance that contains no task or
  alternate goal. The command publishes milestone, assumption, completion, and artifact claims to a
  Maildir-style `tmp` then `new` spool. A replaceable controller collector pulls each declared file
  individually into quarantine, validates it, streams it under fixed quotas, computes its digest,
  stores immutable per-Job content-addressed bytes, appends one immutable provenance-labelled timeline
  event as the acceptance commit, and writes an acknowledgement. Reports require the exact current
  Job, Assignment, and absolute Room outbox scope. `inspect` reads accepted documents only; it never
  ingests or restarts the collector.
- **Delivery and recovery:** Report IDs are independent of content. Ingestion is at least once and
  idempotent by Job plus report ID; a retry with different accepted content is rejected. A collector
  crash before the timeline event leaves only disposable quarantine or an unreferenced blob. A crash
  after the event but before acknowledgement reuses that event and completes the acknowledgement.
  Event files are sequenced under a local Job lock and atomically renamed into the timeline. Automatic
  process recovery composes with #128; the durable Room spool does not depend on one collector process.
- **Provenance:** Worker summaries and artifacts remain claims after safe ingestion. Runtime-observed
  lifecycle/turn outcomes and workflow-observed command exits are facts. Artifact digests bind the
  exact accepted bytes but do not validate their interpretation. Runtime event kinds and artifact
  metadata remain generic; coding-specific check, benchmark, diff, branch, and PR meaning stays in the
  coding workflow.
- **Security and limits:** Accept only fixed-schema manifests, bounded one-line summaries, safe display
  names, sequential fixed spool names, and regular files. Reject traversal, links, special files,
  archives-as-transport, excessive file count, files over 100 MiB, and Jobs over 500 MiB. Transfers do
  not follow links and stream to controller-owned quarantine rather than buffering artifact contents.
  The Worker never chooses a controller destination path.
- **Research basis:** Controller file APIs in [Incus](https://linuxcontainers.org/incus/docs/main/howto/instances_access_files/)
  and [E2B](https://e2b.dev/docs/quickstart/upload-download-files) support provider-mediated copy
  rather than shared authority. [Maildir](https://cr.yp.to/proto/maildir.html) demonstrates complete
  publication through temporary files and atomic rename. The
  [transactional outbox](https://docs.aws.amazon.com/prescriptive-guidance/latest/cloud-design-patterns/transactional-outbox.html)
  and [Temporal idempotency guidance](https://temporal.io/blog/idempotency-and-durable-execution)
  support stable identities and idempotent at-least-once consumers. Bazel's
  [CAS model](https://bazel.build/remote/caching) separates digest-addressed bytes from metadata;
  [in-toto](https://github.com/in-toto/attestation/blob/main/spec/README.md) supports binding subjects
  to provenance; and [OpenHands events](https://docs.openhands.dev/sdk/arch/events) reinforce explicit
  immutable source attribution. OWASP upload guidance motivates strict path, link, type, and quota
  validation.
- **Rejected alternatives:** Reject writable authoritative mounts and writable host staging mounts
  because they weaken the boundary and couple the design to local Incus. Defer Codex dynamic tools or
  MCP as the primary transport because they add harness-specific protocol work and do not carry large
  artifacts. Do not adopt Temporal/Postgres, a global object store, full in-toto signing, transcript
  scraping, or archive extraction for this local-first slice; each adds machinery or false authority
  without improving the current coding workflow.
- **Compatibility:** Timeline/report schemas, quotas, collector process shape, Room paths, capability
  wording, and rendering are internal and replaceable. The enduring boundary is detached approved
  context inward, validated claims outward, immutable provenance, and no writable authoritative mount.
- **Reconsider when:** A remote Room needs push delivery instead of polling, a second harness proves a
  smaller report-tool seam, artifact volume justifies a global store and garbage collector, signed
  attestations become a real workflow requirement, or fixed quotas block observed coding evidence.
