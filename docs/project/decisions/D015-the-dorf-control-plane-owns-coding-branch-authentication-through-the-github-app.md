# D015: The Dorf control plane owns coding-branch authentication through the GitHub App

- **Applicability:** historical
- **Areas:** github, deployment, sandboxes
- **Read when:** Changing GitHub credential ownership, token minting, or delivery into a Sandbox.
- **Decision history:** Retained pre-consolidation decision.; application contracts retired by D144 — 2026-09-18.
- **Decision:** The Dorf control plane owns coding-branch authentication through the GitHub App.
- **Why:** It delivers short-lived installation tokens through the Environment seam without borrowing ambient controller-machine Git credentials, credential stores, or checkout state.
- **Reconsider when:** Another source-control host is supported or a narrower equally usable credential flow is proven.
