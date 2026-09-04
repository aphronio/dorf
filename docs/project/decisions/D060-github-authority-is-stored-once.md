# D060: GitHub authority is stored once

- **Applicability:** current
- **Areas:** github, persistence, workflows
- **Read when:** Changing where GitHub repository, Proposal, or terminal Outcome authority is stored.
- **Decision history:** Accepted Proposal/Outcome simplification — 2026-08-10
- **Decision:** The Job owns immutable GitHub repository, installation, base branch, and head branch
  authority. Proposal retains only pull-request number, URL, exact proposed Revision, and body digest.
  Outcome retains disposition and observation time. Accepted and rejected Outcomes additionally retain
  the exact terminal pull-request observation and optional merge commit. Explicit abandonment is its own
  human authority: it may precede Proposal and therefore retains no invented GitHub state; when a Proposal
  already exists, Dorf still verifies its exact identity and refuses a merged pull request. Publication and
  observation join these facts instead of copying authority or an already-validated remote head. Merge and
  close are observed automatically; `dorf job abandon JOB` is the only manual Outcome command.
- **Why:** Proposal and Outcome repeated facts fixed by their Job and predecessor, creating comparison
  code, larger schemas, and inconsistent states that had no product meaning. One owner per fact makes
  publication, inspection, and terminal recovery read in the same order as the workflow.
- **Reconsider when:** A Proposal must survive independently of its Job, one Job can own multiple live
  GitHub authorities, or a terminal authority contains a new fact that has no current owner.
