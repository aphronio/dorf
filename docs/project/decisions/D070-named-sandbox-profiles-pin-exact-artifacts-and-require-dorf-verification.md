# D070: Named Sandbox profiles pin exact artifacts and require Dorf verification

- **Applicability:** partial
- **Areas:** sandboxes, harnesses, deployment
- **Read when:** Changing named Sandbox profile definitions, selection, verification, or provider artifact custody.
- **Decision history:** Accepted incremental profile-management slice; base file contract refined by D089 and
  online re-verification refined by D091; Incus endpoint custody refined by D101 — 2026-08-26;
  remote Incus profile terminal proved — 2026-08-27
- **Decision:** PostgreSQL owns named Sandbox profiles. A profile binds one provider, exact provider
  artifact, Harness, provider networking and lifecycle settings, and Dorf verification receipt.
  Provider credentials and host paths remain deployment configuration and never enter the profile.
  One Deployment owns at most one Incus endpoint and client identity. An Incus profile binds that
  endpoint's stable public identity and owns its restricted project, storage pool, network, exact
  image and disk contract, and exact guest-reachable Provider Gateway URL; it never borrows ambient
  Incus CLI context. The same endpoint contract admits a local Unix socket or remote HTTPS daemon,
  and D101 owns the fixed supported remote topology.
  One verified profile may be selected explicitly per Job; omission resolves the one deployment
  default. The Job durably pins the profile name. Workers resolve that name through the composition
  root into the existing provider-neutral Sandbox and Harness contracts, so workflows contain no
  Incus, E2B, or Harness selection branches.
- **Artifact boundary:** `profile create` adopts an existing provider artifact; it does not build one.
  Incus aliases are resolved to an exact image fingerprint before persistence. E2B profiles require
  an exact template-build reference. `profile install` is the official Incus release convenience and
  creates the same ordinary profile after verified import. `profile add` is the post-setup guided
  convenience: it reuses provider access and the model Gateway already owned by the Deployment,
  selects Dorf's official provider artifact, creates or resumes the same ordinary profile, runs its
  mandatory verification and cleanup, and preserves an existing default unless explicitly changed;
  the first verified profile becomes the default. Missing provider access or Gateway configuration
  returns to `dorf setup`; it never moves those facts or credentials into the profile.
  Bring-your-own artifacts receive no Dorf provenance or security attestation merely by being
  admitted.
- **Verification boundary at acceptance (refined by D089):** Dorf owned one mandatory, versioned
  `base-1` functional probe before a profile could become default or admit a Job. The explicit, potentially billable operation reconciles
  one durably owned disposable Sandbox, verifies a writable workspace, the baseline atomic file
  operation, `bash`, `git`, `rg`, and the selected Harness/version, then ownership-deletes the
  Sandbox and confirms absence. Its stable
  ownership tuple and typed receipt make process-loss recovery converge through cleanup rather than
  leak a proof resource. Each explicit `profile verify` starts a fresh functional attempt after any
  prior settled receipt; convergent setup reuses an exact current successful receipt instead of
  making another billable provider call. An interrupted attempt resumes its exact cleanup. Ordinary
  Job admission uses the same latest successful receipt. D091 separates that admission receipt from already-admitted runtime
  authority and permits a fresh attempt against the unchanged definition while Jobs remain open.
  If a provider definitively reports that the pinned artifact no longer
  exists, Core invalidates the receipt for new admission, leaves the affected Job at its exact
  current fact with actionable attention, and completes that task attempt without retrying the
  unchanged create. Cleanup may still resolve the pinned profile so owned resources remain
  releasable. Repository-specific dependencies remain the repository setup or custom
  artifact's responsibility. Optional capabilities stay broad and are added only when an actual
  workflow requires them.
- **Mutation rule:** `profile update` applies only explicitly supplied fields to the latest locked
  definition; provider changes require a new named profile. A changed profile may be updated only
  while no Job using its name has incomplete cleanup, and the change clears its default and
  verification receipt, forcing explicit re-verification. A no-op patch preserves both. Exact
  profile revisions are deliberately deferred; immutable-while-in-use is sufficient until measured
  operations require historical profile definitions beyond completed Jobs.
- **Proof:** Live E2B dogfood preserved the exact verified definition on a no-op, invalidated only a
  changed Gateway field, failed setup until re-verification, then restored and investigated a
  retained unpublished commit after its source checkout was removed. Cleanup left zero matching E2B
  Sandboxes. The investigation caught an update resetting public `created_at`; the SQL now preserves
  creation time and PostgreSQL integration guards it.
- **Supersedes:** D067's process-wide `DORF_SANDBOX_PROFILE`/`DORF_HARNESS` selection and
  differently-configured-worker fence. The durable Job fence is now the named profile itself, and
  every Worker may recover Jobs across configured Incus and E2B profiles when their deployment
  credentials are available.
- **Why:** Provider kind alone did not identify the exact image, Harness, network, or verified
  runtime contract, and process-wide selection prevented one deployment from truthfully recovering
  Jobs on different providers. Named profiles improve operator UX and retain the narrow adapter seam
  without introducing a provider matrix, plugin registry, image builder, or fine-grained tool claims.
- **Reconsider when:** A real workflow earns browser, nested-container, served-endpoint, snapshot, or
  GPU admission; completed Job audit needs immutable historical profile definitions; or a provider
  cannot support the stable verification ownership and cleanup contract.
