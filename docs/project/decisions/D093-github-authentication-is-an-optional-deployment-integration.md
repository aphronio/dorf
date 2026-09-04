# D093: GitHub authentication is an optional deployment integration

- **Applicability:** current
- **Areas:** github, deployment, workflows
- **Read when:** Changing GitHub App setup, credential custody, or repository token minting.
- **Decision history:** Accepted module boundary refinement — 2026-08-24
- **Decision:** Refine D015's coding-branch authentication framing into one optional GitHub
  integration composed beside Core. The deployment owns one default protected GitHub App credential
  bundle and uses it to mint short-lived repository-scoped tokens. `dorf setup` remains the shared
  control-plane foundation; `dorf integration github setup` creates the App through GitHub's App
  Manifest flow using one static, backend-free GitHub Pages launcher. GitHub redirects back to that
  page, which displays the short-lived conversion code for manual transfer to the waiting CLI; Dorf
  verifies the returned App contract, atomically installs the returned identity and private key,
  then directs the operator to the
  reusable installation URL and waits for explicit completion. One bounded observation through the
  authenticated App authority must find at least one installation before setup reports the
  integration ready. It runs no callback listener, hosted relay, polling loop, background service,
  or scheduler and accepts no repository or workflow scope.
- **Permissions and verification:** App registration uses the fixed envelope supported by this
  module: metadata read, contents write, issues read, and pull-requests write. Setup verifies the App
  owner, identity, exact permission envelope, and absence of subscribed events before retaining it;
  a rerun proves that retained contract and the presence of an installation remotely, returns ready
  without terminal input when both exist, and otherwise resumes the configured App's installation
  step. Runtime operations own exact
  repository discovery, base or Revision proof when needed, and least-scope repository token minting
  within that envelope. The resulting installation and repository facts remain authority of the
  operation or durable consumer that needs them. The selected profile's coding runtime composes the
  deployment-default integration; neither the durable profile nor the Job request stores its
  credentials or permission envelope. Replacing the default App requires explicit `--yes`;
  credential-free access to a public Git repository needs no GitHub App.
- **Authority:** This does not move repository authority into deployment configuration. D060 remains
  the coding Job authority for its immutable repository, installation, base, and head. Another
  workflow or direct client owns its own accepted per-use scope while reusing the same integration.
- **Why:** GitHub authentication and token minting are useful outside one native workflow, but are
  not generic execution custody. Separating deployment credentials from consumer scope makes the
  reuse explicit without leaking GitHub into Core or treating global setup as workflow setup.
- **Reconsider when:** Multiple App identities per deployment are required, another source host
  earns a common integration contract, a Dorf web UI or cloud control plane can absorb the browser
  handoff behind an authenticated callback, or a real consumer needs authority that cannot be
  expressed as an exact repository, installation, and native permission floor.
