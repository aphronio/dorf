# Remote Control API Implementation Slices

This is the high-level execution plan for the
[Remote Control API Design](control-api-design.md). That design owns the intended vocabulary,
external behavior, and deferred scope. This document deliberately avoids prescribing packages,
tables, helper types, or internal call graphs; each slice must first discover the smallest truthful
shape in the code that exists when the slice starts.

This plan is temporary implementation guidance. Its status markers record completed implementation
and proof, not release availability.

| Slice | Status |
| --- | --- |
| 1 — One authenticated remote direct Job | Delivered and dogfood-proven on 2026-08-26 |
| 2 — Complete the direct Job interaction loop | Delivered and dogfood-proven on 2026-08-26 |
| 3 — Typed workflow admission | Delivered and dogfood-proven on 2026-08-26 |
| 4 — Automation-quality contract and operations | Planned |

## Required method for every slice

### 1. Discover broadly before designing locally

Before editing, trace the complete affected path: public CLI and presentation, composition root,
Core application handles, direct and workflow adapters, PostgreSQL facts and transactions, Absurd
recovery, provider and Harness adapters, setup and deployment lifecycle, documentation, and existing
tests. Run focused read-only scouts in parallel when the questions are independent.

The discovery must identify:

- the current authority for identity, lifecycle, ordering, recovery, cleanup, and credentials;
- primitives already capable of the desired behavior;
- duplicate queries, projections, validations, state, adapters, and compatibility paths;
- failure and process-loss boundaries, including committed-but-response-lost behavior;
- the narrowest client-visible result that can be proved end to end.

Write a short design checkpoint before implementation. Explicitly ask whether the proposal creates a
second Job identity, lifecycle, status, retry system, event history, credential authority, workflow
engine, or client hierarchy. Prefer using or improving an existing primitive over wrapping it in a
parallel abstraction. If the cleanest slice requires correcting an existing seam, make that
correction rather than building around a bad boundary.

### 2. Deliver one complete vertical result

A slice ends in behavior a real remote client can use. A schema, transport router, generated model,
mock adapter, or internal helper is not a terminal. Keep policy with its existing owner and expose
only the application capability the client needs.

Do not add registries, plugin contracts, generalized resource frameworks, event stores, or
compatibility layers for hypothetical later slices. Prefer boring HTTP and standard-library or
already-owned repository primitives.

### 3. Make every test earn its keep

Add the minimum strong tests that protect a plausible regression in authority, security,
idempotency, concurrency, recovery, cleanup, protocol compatibility, or exact-byte behavior. Prefer
one public-boundary behavior proof, one fault or PostgreSQL integration proof where durable facts are
changed, and targeted live dogfood over many helper-shaped unit tests.

Do not retain tests merely because code was moved. Delete tests that only freeze callbacks, helper
calls, generated shapes, duplicated validation branches, prose, field inventory, or speculative
combinations. A test earns its cost only if its failure would identify a material product defect.

### 4. Simplify after implementation

Every slice requires a distinct simplification pass before it is accepted. This is not optional and
is not limited to the new files.

- Measure handwritten production, generated, test, and documentation changes.
- Search for stale commands, APIs, fields, queries, branches, compatibility paths, and duplicate
  authorities made obsolete by the slice.
- Challenge every new DTO, interface, callback, state value, dependency, and test.
- Prefer deleting an old path over retaining two ways to perform the same operation.
- Allow an architectural rewrite or boundary correction when it produces a smaller, clearer system
  with the same proven behavior.
- Repeat focused proofs after deletion; do not confuse a large diff or a large test suite with
  confidence.

The slice report must state what was deleted, its final line delta by category, why remaining
additions earn their keep, and any known deferred pressure.

### 5. Prove the real boundary

Run the repository verification contract. Run the PostgreSQL-backed suite when durable facts,
transactions, sequencing, or recovery change. Finish with a disposable real-client proof matching
the changed authority: HTTPS, process loss, Sandbox, Harness, exact files, authentication,
reconnection, or cleanup as applicable. Remove all proof credentials and resources and verify the
unrelated deployment inventory is unchanged.

## Slice 1 — One authenticated remote direct Job

**Status:** Delivered and dogfood-proven on 2026-08-26.

**Client result:** From another machine, a person enrolls one CLI Client, connects to the single Dorf
Deployment, admits a direct Job, inspects it, and requests cleanup without SSH.

This slice establishes the smallest HTTPS discovery, authentication, error, receipt, and Job
representation contracts needed by that journey. The delivered routes and commands are recorded in
the design's [resource model](control-api-design.md#resource-model) and
[CLI vocabulary](control-api-design.md#cli-developer-and-agent-experience). The CLI generates
request identity before admission and hides it from the ordinary human flow. Client configuration
and its client-generated credential live in one dedicated owner-only file rather than deployment
configuration or a new keyring abstraction.

The real HTTPS proof enrolled a remote CLI, discarded the first admission response, recovered the
same Job through exact replay, completed a Turn through the real Sandbox and Harness, and observed
the same canonical Job after independent worker and API restarts. Cleanup completed after another
worker restart, and revoking the Client caused the next authenticated request to be denied. No
provider, Harness, database, or Gateway credential crossed the client boundary.

The API and worker were separately supervised for the proof; this did not claim managed packaging.
Installing, upgrading, diagnosing, and supervising production services remains Slice 4. Current
operator limits are documented in [Getting started](../getting-started.md#3-connect-one-remote-cli-client).

Do not add workflow admissions, watch streaming, named contexts, multi-user identity, or a browser
UI in this slice.

## Slice 2 — Complete the direct Job interaction loop

**Status:** Delivered and dogfood-proven on 2026-08-26.

**Client result:** A remote person or agent can watch a Job, send follow and steer Messages, retry
eligible failed work, retrieve an exact Sandbox file, inspect Evidence, and request cleanup using
stable machine output.

This slice proves snapshot observation and reconnect, invariant Message semantics, exact Sandbox
file bytes, typed recovery guidance, and idempotent lifecycle mutations. The stream remains a view of
canonical Job snapshots rather than a second durable event system.

The real HTTPS proof admitted an early follow that later completed on the retained Thread, steered
one exact active Turn, and received typed `steer_unavailable` after that Turn became terminal rather
than creating a fallback Turn. A JSONL watch reconnected after the API process restarted and
continued from canonical snapshots. The remote client downloaded a real five-byte Sandbox file and
independently matched its SHA-256 digest; cleanup immediately returned typed `file_unavailable`, then
durably revoked the model route, deleted the Sandbox, and completed. Explicit retry returned typed
`retry_unavailable` when no failed execution was eligible, while the live PostgreSQL fault proof
covered successful caller-keyed retry and exact replay. The direct Job truthfully returned no
Evidence; nonempty verified Evidence remains part of Slice 3's coding proof. The proof Client was
revoked, its next request was denied, and all temporary credentials, services, tunnel, and Sandbox
resources were removed.

The final simplification pass removed the old non-atomic retry path, the former Job-scoped file
grammar, a duplicate Message-observation seam, direct-only policy from Sandbox reads, leaked
Task-adjacent response fields, and redundant transport tests. From the Slice 1 checkpoint, the final
delta is: handwritten production Go `+1,411/-133`, authored SQL `+23/-0`, generated Go
`+70/-0`, tests `+736/-46`, and documentation `+175/-97`. The remaining code
earns its keep by owning the canonical snapshot stream, typed public representations, exact-byte
verification, invariant Message translation, and atomic caller-keyed retry. Snapshot polling and
whole-file buffering are accepted current pressure; file writes/listing, an event log, OpenAPI, and
the complete error catalog remain outside this slice.

Do not add file writes, listing, archives, webhooks, or an externally replayable event log.

## Slice 3 — Typed workflow admission

**Status:** Delivered and dogfood-proven on 2026-08-26.

**Client result:** The same authenticated API and CLI can admit and inspect coding and codebase
investigation Jobs while preserving their typed contracts and existing runtime behavior.

This slice adds exactly two workflow-specific admission resources over the same Job, Message,
observation, error, retry, file, Evidence, and cleanup contracts. One closed flat Job union projects
`direct`, `coding`, and `codebase-investigation` without a generic workflow payload. Deployment
Profiles and integration authority remain server-side. Remote investigation accepts only a
credential-free HTTPS repository at an exact Revision; deployment-host retained bundles remain
local input.

The real HTTPS investigation admitted an exact Git Revision, completed an early sequence-2 Follow on
the same workflow and retained Thread, and fetched the 5,105-byte `REPORT.md` with a verified digest.
The Job remained open and idle; explicit cleanup immediately fenced another read with typed
`file_unavailable` (`409`) before cleanup completed.

The real coding proof replayed identical admission to the same Job and rejected changed input with
typed `idempotency_conflict` (`409`). Its deterministic branch produced pull request #154 containing
the one expected documentation file and nonempty verified `git-revision` Evidence. Closing the pull
request unmerged and deleting its branch produced the observed rejected Outcome; the workflow's
conditional cleanup request then revoked the route and deleted the Sandbox. The verified Evidence
remained readable after cleanup.

The final simplification kept each workflow's validation and policy in its existing module, reused
those typed admission services from both local and remote adapters, and deleted the single-kind Job
projection, duplicate outcome/report fields, and false hard-coded scheduling receipts. No workflow
registry, shared nullable payload, generic workflow schema, or workflow DSL was introduced. From the
Slice 2 checkpoint, the final delta is: handwritten production Go `+1,317/-343`, authored SQL
`+0/-0`, generated Go `+0/-0`, tests `+948/-221`, and documentation `+208/-93`. The remaining
code earns its keep by owning the two concrete admission policies and their closed typed Job
representations. Future workflow-revision upgrade and drain compatibility remains unearned: the
public union currently recognizes only the exact compiled revisions.

## Slice 4 — Automation-quality contract and operations

**Client result:** A human, shell script, and coding agent can depend on the same documented API and
CLI behavior without hidden terminal or deployment assumptions.

This slice completes the compatibility surface that real dogfood has earned: OpenAPI publication,
pagination, a published Problem Details catalog and compatibility proof, consistent JSON and JSONL
behavior, Client credential lifecycle, and operator diagnostics for the managed API and worker
services. It reconciles setup, support, release, and agent guidance with the now-proven remote path.

The proof must include generated-client or direct-HTTP use from the OpenAPI description, non-TTY
operation, pagination continuity, authentication expiry/revocation, and a clean install or upgrade of
the managed deployment services.

Do not add MCP, A2A, a hand-written SDK family, OIDC, RBAC, multiple saved Deployment contexts,
webhooks, or a web UI. Let subsequent real clients decide which of those is the smallest next step.

## Completion

After the final slice, replace temporary implementation narration with concise shipped product,
architecture, API, setup, support, and Agent Guide authorities. Preserve consequential decisions and
reconsideration triggers; archive or delete this plan and any superseded design text rather than
leaving a second roadmap behind.
