# D041: Host setup is capability-first with narrow native-package recipes

- **Applicability:** historical
- **Areas:** deployment, sandboxes
- **Read when:** Reviewing the former capability-first host setup and native package recipes.
- **Decision history:** Superseded by D101 — 2026-08-26
- **Decision:** Accept any x86_64 Linux host whose local Incus daemon is already usable, but perform
  automatic package, service, and root-equivalent group mutations only through exact clean-host
  validated recipes. The Go cutover retains Ubuntu 24.04 LTS as the automatic clean-machine recipe;
  it uses native distribution packages, systemd's `incus.service`, and `incus-admin`, while pristine daemons
  delegate storage and private-network creation to `incus admin init --minimal`. Setup reinspects
  real state on every run and requests approval before package, service, or group mutation. Arch and
  other distributions remain capability-first: their operator follows the upstream/native Incus
  installation path and reruns the same readiness command afterward. This narrows the support claim
  rather than carrying a deleted Python host recipe into the Go product.
- **Storage default:** Retain Incus's directory-backed minimal default for the first stranger path.
  In a clean nested Ubuntu 24.04 host, three cached-VM guest-readiness samples had a 15.490-second
  median on `dir` and a 12.425-second median on a disposable loop-backed Btrfs pool. That gain does
  not yet justify installing another filesystem tool or choosing storage on the user's behalf.
- **Why:** Dorf should provide one calm setup experience without becoming a package manager or
  filesystem provisioner. Capability-first inspection preserves portability for users who already
  operate Incus; small evidence-backed mutation recipes keep the recommended path resumable and
  supportable. Delegating initialization to Incus and preferring the least invasive storage choice
  minimizes maintenance and host risk.
- **Reconsider when:** Another distribution completes the clean-host terminal; Incus publishes a
  reviewed universal daemon installer; native package/service semantics diverge enough to require a
  different recipe; or promoted Dorf-image measurements on non-nested supported hosts repeatedly
  exceed ten seconds for warm Room readiness and prove storage is the dominant cost.
