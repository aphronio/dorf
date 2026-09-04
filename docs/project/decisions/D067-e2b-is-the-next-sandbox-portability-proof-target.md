# D067: E2B is the next Sandbox portability proof target

- **Applicability:** current
- **Areas:** sandboxes, harnesses, deployment
- **Read when:** Changing the E2B adapter, provider-neutral Sandbox contract, or second-provider deployment proof.
- **Decision history:** Accepted; complete second-provider Codex and Pi coding-to-PR terminals proven —
  2026-08-14; Incus endpoint and route custody refined by D070 and D101 — 2026-08-26
- **Decision:** Pursue E2B as D063's second Sandbox provider one earned slice at a time. The first
  implementation is a narrow native Go control-plane client for one create attempt, exhaustive exact
  ownership discovery across running and paused Sandboxes, individual-resource attestation, and
  ownership-guarded deletion. The second implementation vendors one pinned upstream process schema,
  generates its protobuf and Connect-Go client, and adds only Dorf's required argv, stdin, raw output,
  terminal-status, timeout, cancellation, and explicit-kill semantics. It consumes a configured
  prebuilt template reference and does not build templates, transfer files, expose a general provider
  registry, or enter common workflow code. E2B's official JavaScript SDK remains a behavioral oracle,
  not a runtime sidecar or a second Dorf control plane.
- **Common seam:** Incus, E2B, and any later Sandbox provider must satisfy one provider-neutral
  Sandbox contract selected by the admitted profile, just as Codex and Pi satisfy one Harness
  contract. That contract preserves Dorf's stable ownership identity while treating the provider's
  resource locator as opaque; it separates one mutation attempt from read-only reconciliation,
  exposes the bounded command and authenticated-endpoint primitives proven by real consumers, and
  keeps deletion ownership-guarded. Provider-specific lifecycle APIs, envd, Incus commands,
  networking, and connection headers remain inside adapters. Capability admission may reject an
  unsupported Harness/provider combination without adding provider branches to workflow or common
  consumer code. Extract the exact interface only after the E2B endpoint proof completes the second
  implementation's required surface; do not build a provider registry or speculative capability
  matrix.
- **Extracted seam:** `internal/sandbox.Sandbox` is the single contract used by terminal,
  repository, publication, Codex, and Pi. Every execution, endpoint, review, repository, and cleanup
  call carries the complete Dorf ownership tuple; common code never guesses an E2B provider ID or
  relies on the Dorf Sandbox name without its nonce. Incus resolves its deterministic instance while
  E2B exhaustively resolves one opaque provider locator from exact metadata. Authenticated endpoints
  expose separate guest-bind and controller-dial URLs plus cloned provider headers without exposing
  their capability in formatting. Provider-route reachability is also adapter-owned: each Profile
  provides one exact guest-reachable Gateway URL, while E2B specifically requires HTTPS and defaults
  to allowing only that hostname over otherwise denied egress. A selected repository profile may
  explicitly admit general internet egress. Tunnel lifecycle remains deployment-owned. A
  compile-time implementation assertion covers both adapters and an
  import-boundary test rejects Incus or E2B imports from common consumers. Startup selects exactly
  one configured profile and injects its adapter through the common seam. Admission durably pins the
  Job's Sandbox profile; a worker configured for another profile leaves ordinary work or cleanup in
  observable attention before any provider call. This is a scalar authority fence, not a provider
  registry. The admitted E2B profile supports both Codex and Pi without provider branches in shared
  consumers. The proved coding-to-PR terminals do not make the disposable-tunnel profile a supported
  deployment.
- **Recovery boundary:** E2B create has no caller-selected resource ID or documented idempotency key.
  Dorf therefore attaches its Job ID, durable Sandbox ID, and ownership nonce as provider metadata.
  A create request is never automatically replayed after possible dispatch. Reconciliation
  exhaustively queries the provider by that exact identity: one match is adoptable, no match remains
  absent or uncertain according to the caller's durable Action state, and multiple matches require
  attention. Provider-generated IDs remain opaque locators. Cleanup re-attests exact ownership before
  deletion and confirms absence separately.
- **Proof:** A live Hobby-account test let E2B accept a restricted Sandbox create and then discarded
  the successful response locally. The Go client recovered the single resource by exact ownership
  metadata, inspected it by its opaque provider ID, deleted it, and confirmed no ownership match
  remained. Hermetic tests cover the same lost-response path, pagination, duplicate ownership,
  foreign-resource deletion refusal, stale locators, idempotent absence, and provider errors without
  retaining E2B's returned access tokens. A second live test used native binary ConnectRPC against
  envd 0.6.13 and proved literal argv, binary stdin, separate raw stdout and stderr, exact nonzero
  exit status, provider process timeout, caller-cancellation ambiguity, and one-shot PID cleanup. One
  shared guest recipe then produced exact E2B build
  `dorf-debian13-combined:8bb1103a-c331-4098-9dac-b26a2ed31eae` from clean source
  `6e5801b029608656f5b6c2076032befafed4b990`; the native Go adapter qualified its Debian 13 amd64
  identity, envd 0.6.13, complete tool inventory, Codex 0.147.0, Pi 0.84.1, root workspace, and
  credential absence before ownership-guarded deletion. The account had no running or paused
  Sandbox afterward. An authenticated endpoint proof then separated Codex's `ws://0.0.0.0:4500`
  process bind from E2B's provider-resolved `wss` URL, restricted public ingress, and required both
  E2B's scoped traffic header and Codex's independent control capability. After abrupt controller
  loss, a second connection resumed the exact native thread and observed the exact submitted turn;
  no upstream model outcome was claimed. Every proof Sandbox, including failed iterations, was
  ownership-deleted and final account inventory was empty. After extracting the common seam, the
  live proof was rerun entirely through `internal/sandbox.Sandbox`: it reconciled creation without a
  provider locator in Codex, executed the controller commands, resolved the authenticated endpoint,
  recovered the exact thread and turn after abrupt loss, ownership-deleted the resource, and observed
  it absent. The next live proof kept the local broker on its private Incus bridge and placed a
  disposable outbound HTTPS tunnel in front of it. E2B denied all egress except that exact hostname;
  an unauthenticated request from the Sandbox returned 401, common `terminal.Externals.RouteCreate`
  installed only the scoped route key and external URL, and an unchanged Codex Harness completed a
  real `gpt-5.6-sol` turn through the owner Gateway. Cleanup revoked the exact route before removing
  its Sandbox files, ownership-deleted the Sandbox, observed zero matching E2B resources and zero
  proof routes, and stopped the tunnel. No tunnel hostname, route key, E2B key, or upstream credential
  is durable repository state.
  Provider/profile composition then admitted E2B for one durable no-change Codex Job. The common
  workflow created one E2B Sandbox, cloned the exact remote Revision, completed repository setup,
  installed the scoped route, bound and completed one real Codex thread and turn, and recorded exact
  unchanged Git-revision Evidence. The first delivery attempt exposed a missing `sandbox_id` in the
  PostgreSQL delivery projection; after adding that durable field, the same Job reconciled the
  existing Sandbox and route rather than replacing either. Explicit cleanup recorded route revoke
  before ownership-guarded Sandbox delete, completed its Absurd task, and an independent inventory
  check found zero exact E2B matches and zero Gateway routes. The disposable tunnel was then stopped.
  The Job used explicit general internet egress because clean repository setup follows package and
  redirect hosts that are not a stable static allowlist; the model route remained consumer-scoped.
  Two modifying Jobs then completed the remaining terminal. The first produced exact Revision
  `3183e618329abd72f0697fd3a9f83e86a94dc91d`, passed both repository Checks, correctly selected no
  agent review for a documentation-only change, published PR #134, observed its closed rejection,
  and cleaned its route and Sandbox. The second produced exact Revision
  `764f9312dd217b149e393c0ae1f31696894f4174`, passed both Checks, and selected one general reviewer.
  A separately owned E2B Sandbox checked out the exact Revision, ran Codex with immutable read-only
  capability, and returned no-material-issue feedback with Revision-bound review Evidence.
  The original implementation thread accepted that feedback without changing the Revision, Dorf
  published PR #135, observed merge commit `f6961465f89b383d3ae0e84a207f64c4f2fba925` as an accepted
  Outcome, then revoked each route before ownership-deleting its main and reviewer Sandboxes. Both
  Absurd tasks completed, an independent E2B metadata query found zero Job-owned running or paused
  Sandboxes, the Gateway route inventory was empty, and the disposable tunnel stopped. No provider,
  route, tunnel, GitHub, or upstream credential entered durable evidence or repository state.
  Detailed observations remain in the
  [E2B capability proof](../../research/e2b-capability-proof.md).
  A later live E2B Pi Job completed implementation and isolated-review AgentRuns in separately owned
  Sandboxes, exact-Revision Checks, host-side publication, a terminal Outcome, and cleanup through the
  same common workflow. D068 records the exhausted-attempt recovery observed during that terminal.
- **Next earned boundary:** Choose and prove a stable deployment-owned tunnel/domain only when the
  operational E2B profile is ready. Do not add files, snapshots, waiting-Sandbox lifecycle
  optimization, a provider registry, or a capability matrix until a concrete workflow earns it.
- **Why:** E2B passed the bounded VM, Docker Compose, browser, authenticated endpoint, pause/resume,
  network, and cleanup capability spike without a fixed subscription. A handwritten lifecycle
  client keeps Dorf's reconciliation policy visible and small; adopting the full high-churn
  JavaScript SDK through a sidecar would add another protocol and process boundary before command
  streaming has proved that cost necessary.
- **Reconsider when:** The native command protocol cannot remain smaller than a maintained sidecar,
  E2B cannot support Dorf's no-duplicate recovery or scoped Provider Gateway route, deployment-service
  cost or reliability fails dogfood, or another provider reaches the complete D063 coding-to-PR
  terminal with materially less authority overlap.
