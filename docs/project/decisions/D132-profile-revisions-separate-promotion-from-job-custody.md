# D132: Profile revisions separate promotion from Job custody

- **Applicability:** current
- **Areas:** sandboxes, persistence, deployment
- **Read when:** Updating a profile used by retained Jobs, promoting verified images, or resolving old runtime custody.
- **Decision history:** Replaces D070 and D091's immutable-while-in-use mutation fence, 2026-09-15.
- **Decision:** Store exact profile definitions as immutable content-hashed revisions. A stable name
  holds candidate and active pointers. Updates stage candidates; successful functional verification
  and confirmed verification-Sandbox deletion promote the current candidate atomically. Preserve
  the existing default and active selection when staging or when a candidate fails.
- **Custody:** Each Job pins its revision in the admission transaction. All runtime and capability
  resolution, inspection, recovery, and cleanup use that binding, including after process restart.
  Replaying an admission key does not select a newer revision. Historical definitions remain
  available after cleanup. An unavailable old artifact fences only its revision's new admissions.
- **Concurrency:** Lock the name before reading active verification with a fresh statement snapshot.
  One joined locking query can wait for promotion and then recheck its old joined receipt against
  the new pointer, spuriously rejecting valid admission. Staging, verifier ownership, and promotion
  retain serialization; pending verifier cleanup still blocks staging another candidate.
- **Why:** A profile name serves future selection; a retained Job needs exact custody. Making the
  name itself immutable while any Job lives prevents image delivery for persistent conversations
  and forces unnecessary Job cleanup. Separating those responsibilities removes that dependency.
- **Scope:** This changes future admission, not packages inside running VMs. Future package
  activation must separately manage process boundaries and native-session compatibility. Migrated
  Jobs bind the profile definition still present at migration time; earlier overwritten definitions
  cannot be recovered from the old schema.
- **Proof:** PostgreSQL integration covers staging with incomplete Jobs, failed candidates,
  cleanup retry, promotion, default preservation, immutable bindings, admission replay, migration,
  concurrent promotion/admission, concurrent staging, and old-revision failure isolation. Runtime integration reloads an
  old Job after image/Harness promotion and resolves all workflow, inspection, and cleanup bundles;
  message-image capability follows each pinned Harness. These checks use disposable PostgreSQL
  and synthetic verification receipts; they do not claim a new live provider-image proof.
