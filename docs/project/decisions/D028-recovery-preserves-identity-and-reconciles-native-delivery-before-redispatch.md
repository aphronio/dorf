# D028: Recovery preserves identity and reconciles native delivery before redispatch

- **Applicability:** historical
- **Areas:** core, sandboxes, harnesses
- **Read when:** Reviewing the replaced Worker recovery and native-delivery reconciliation design.
- **Decision history:** Superseded by D047, 2026-08-06
- **Decision:** `worker recover NAME` is the explicit recovery boundary. It restores the exact current
  Room and provider identity when the Incus VM still exists, including after a host restart. If the
  provider body is absent, it retains Worker and Job identity, records the old Room as absent, creates
  one replacement Room, and rolls an open Job to one new same-Worker Assignment generation. Generic
  Assignment workspace/reporting scope and coding clones are rebuilt in the replacement Room before
  delivery resumes. Replaceable Worker/Job dispatchers and the exact current Assignment collector
  are restarted.
- **Native delivery:** A transport or controller failure after native submission begins records
  `recovery-required`, not failure. Recovery inspects the bound harness thread and uses the recorded
  baseline and native turn IDs to distinguish no submission, one submitted turn, completion,
  interruption, failure, active work, pending approval, and uncertainty. Input is resubmitted only
  after native history proves no turn was submitted. Worker-general and Job-native reconciliation
  remain independent. Native transcript bytes stay harness-owned.
- **Continuity limit:** Room replacement preserves the Dorf conversation binding but cannot
  promise that Codex can load a thread whose harness state existed only in the lost Room. That case
  remains visibly blocked with the native error; Dorf does not copy transcripts or silently start
  a replacement thread and call it continuity.
- **Why:** Clients, dispatchers, collectors, and app-server control processes are replaceable, while
  blind resend can duplicate side effects. Provider disk survival is the strongest available source
  of workspace and native-history continuity. An immutable Assignment generation records a changed
  Room honestly without reassigning the Job to a different Worker.
- **Reconsider when:** The harness offers native idempotency keys or cloud-restorable history, a
  remote control plane needs leases rather than local process locks, or another Environment provides
  a materially different restore/replacement boundary.
