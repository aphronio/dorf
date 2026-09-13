# D121: Messages retain application developer instructions

- **Applicability:** current
- **Areas:** core, client-api
- **Read when:** Changing application instruction custody or Harness prompt authority.
- **Decision history:** Accepted, 2026-09-13. Revises D111 for application instructions and workspace authority.
- **Decision:** Retain generic nullable application developer instruction text on each accepted
  Message. Include it in idempotent replay equality and restore it for fresh Harness submission.
  Clients own generation selection and content; Core owns immutable custody.
- **Authority:** User-owned workspace customizations remain user instructions. Application rules
  use native developer messages without replacing built-in model instructions. A fixed developer
  notice revokes legacy workspace-derived developer instructions before refreshed user context,
  independently of application snapshots. Null preserves the
  prior snapshot; empty explicitly clears it. Only fresh submission applies a snapshot; steering
  and recovery of an accepted Turn leave its prompt unchanged.
- **Why:** Mutable user files cannot own application policy or reproduce the accepted generation
  after a retry. The two instruction sources have different owners and authority.
- **Codex:** Use native thread injection with complete replacement framing. The verified loaded
  thread does not adopt changed developerInstructions supplied to thread/resume.
