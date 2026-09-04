# D017: Managed repositories expose explicit development-tooling contracts

- **Applicability:** historical
- **Areas:** workflows, core
- **Read when:** Reviewing the repository tooling contract removed when coding setup and Checks were deleted.
- **Decision history:** Retained pre-consolidation decision.
- **Decision:** Managed repositories expose explicit development-tooling contracts.
- **Why:** Repo-owned commands and allowlisted environment bindings keep app semantics in the repo without Dorf coupling in product code.
- **Reconsider when:** A compatible repo-owned standard or a repeated cross-repo primitive proves a smaller contract.
