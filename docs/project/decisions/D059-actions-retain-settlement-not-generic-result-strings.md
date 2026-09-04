# D059: Actions retain settlement, not generic result strings

- **Applicability:** current
- **Areas:** core, persistence
- **Read when:** Changing Action result storage, settlement state, or ownership of external facts.
- **Decision history:** Accepted Action-result simplification — 2026-08-10
- **Decision:** An Action retains its stable identity, kind, scope, and settlement state: unsettled,
  succeeded, or failed. The external adapter validates exact identity and authority before recording
  success. Durable facts returned by an operation live in their natural typed owner: setup output in
  Evidence, pull-request identity in Proposal, terminal disposition in Outcome, and exact Sandbox or
  Revision targets in Action scope. Dorf does not copy those facts into generic `external_id` or
  `external_outcome` Action strings.
- **Why:** Generic result strings repeated facts already known from Action scope or a typed product
  record, forced central parsing of adapter-specific formats, and made inspection look authoritative
  in two places. Settlement plus the natural owner states the same recovery story directly.
- **Reconsider when:** A concrete external mutation returns a stable, non-derivable fact required for
  reconciliation that has no honest typed product owner.
