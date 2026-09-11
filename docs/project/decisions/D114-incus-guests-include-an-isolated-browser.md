# D114: Incus guests include an isolated browser

- **Applicability:** current
- **Areas:** sandboxes, harnesses, release
- **Read when:** Changing browser tooling, guest browser persistence, or Incus image proof.
- **Decision history:** Accepted after a retained Codex Sandbox completed browser navigation across Messages — 2026-09-11
- **Decision:** The official combined Incus image includes pinned browser-use, browser-harness,
  Playwright, and Chromium. Browser dependencies use their own Python environment. A systemd
  service starts one headless Chromium process with a fresh profile inside each VM. The plain
  `browser-use` command attaches to that VM's loopback CDP endpoint and starts the service if
  needed. Codex receives the packaged browser-use skill; Pi can use the same CLI and skill.
  Local recordings are enabled and may be disabled through the CLI. Browser profiles and
  recordings belong to the retained Sandbox and disappear on requested cleanup.
- **Boundary:** This is a guest tool capability. Core gains no browser API, browser workflow,
  interpretation of navigation success, or new cleanup policy. The image contains no personal
  browser state, cookies, credentials, recorded actions, or existing Harness sessions. Chromium
  runs as the guest's root user with `--no-sandbox`; the Incus VM provides isolation, and its
  debugging listener binds only to loopback. E2B templates retain their existing contents.
- **Promotion:** The release authority uses the current authenticated Job API and a configured
  deployment worker. It verifies separate Codex and Pi no-change coding Messages and unchanged
  Revision Evidence, browser navigation from Example.com to IANA, and completed cleanup against
  one candidate fingerprint. Native Thread/Turn custody remains the Harness adapter's contract;
  the public proof uses settled Message outcomes rather than removed internal inspection fields.
- **Why:** Agents need interactive web access inside their isolated environment without installing
  a browser on every Job or borrowing the operator's desktop session. The proven retained-VM
  setup provides that capability through existing Harness tools.
- **Cost:** The image includes a browser and a second Python environment. Its size and browser
  security updates become release concerns. Recordings consume guest disk and may contain page
  contents; they are local to the Sandbox.
- **Refines:** D064's Incus tool contents and D066's image proof procedure. The shared provider
  baseline and combined Codex/Pi packaging remain in place.
- **Reconsider when:** Browser storage or startup costs dominate Jobs, the tooling cannot attach
  reliably across Messages, or another provider qualifies the same capability.
