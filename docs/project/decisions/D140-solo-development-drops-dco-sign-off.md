# D140: Solo development drops DCO sign-off

- **Applicability:** current
- **Areas:** release
- **Read when:** Changing contribution certification or commit requirements.
- **Decision history:** Replaces the mandatory DCO policy in CONTRIBUTING.md, 2026-09-16.
- **Decision:** Commits no longer require a `Signed-off-by` trailer. Apache-2.0 contribution
  licensing is unchanged, and authorship remains recorded by ordinary Git commit identity.
- **Why:** The maintainer chooses not to require a separate certification trailer for each commit.
  This reduces contribution ceremony, not build runtime. Authorship and permission to contribute
  still matter regardless of contributor count; a Git identity does not replace DCO certification.
- **Preserved behavior:** Contribution licensing, release provenance, immutable-release verification,
  and required code checks remain unchanged.
- **Reconsider when:** The project's contribution governance requires explicit DCO certification or
  a contributor license agreement.
