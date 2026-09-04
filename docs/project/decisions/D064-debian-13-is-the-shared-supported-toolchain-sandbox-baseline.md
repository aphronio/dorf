# D064: Debian 13 is the shared supported-toolchain Sandbox baseline

- **Applicability:** current
- **Areas:** sandboxes, harnesses, release
- **Read when:** Changing the supported Sandbox guest baseline, shared toolchain contents, or image construction integrity.
- **Decision history:** Accepted profile baseline; combined Harness packaging refined by D066; second-provider
  template qualified by D067 — 2026-08-14
- **Decision:** The official x86_64 Sandbox profile uses an exact provider-native Debian 13 base
  identity and carries only a cross-repository workstation baseline: Python 3.14 with pip, Node 24
  LTS, pinned Go and uv, Git, the verified Harness executables, native C/C++ build tools, and common
  shell/archive/search utilities. npm is used only to install Harness packages during construction; npm, npx, Corepack, Yarn, pnpm,
  pytest, Ruff, application libraries, GitHub CLI, tmux, PostgreSQL, and Absurd are not shared image
  contents. Managed repositories retain their
  language versions and dependencies in ordinary project metadata and lockfiles and install them
  through `commands.prepare`.
- **Integrity:** One provider-neutral, versioned guest recipe is the complete tool and Harness
  construction authority and copies no host state into the fresh base. Provider packaging supplies
  and records its exact Debian identity: Incus uses the resolved `images:debian/13` VM fingerprint;
  E2B uses an exact `linux/amd64` OCI manifest digest. The guest profile metadata records that base
  identity, every installed bootstrap-tool version, each Harness npm integrity, and verified Node,
  Go, and uv archive digests; provider artifact manifests bind the exact build to the recipe and
  source. Provider release/profile proof verifies the resulting artifact and reconciles its
  disposable Sandbox cleanup.
- **Why:** A small polyglot bootstrap lets the selected Harness inspect and deterministically prepare ordinary Python,
  JavaScript, Go, and native-extension repositories without turning their dependency graphs into a
  Dorf release concern. Debian 13 supplies the current stable/LTS runway. Keeping project packages in
  the repository avoids cross-repository version conflicts and preserves the development-tooling
  seam required by D049. Each provider retains its natural packaging path while consuming the same
  recipe: Incus publishes a configured VM instance, while E2B snapshots a template built from an
  exact OCI base. Packaging identity does not enter the shared runtime contract.
- **Cost:** The shared image and its proof surface are larger than the earlier harness-only image, and
  Python, Node, Go, uv, and Harness support windows require deliberate image refreshes. The supported
  clean-machine host remains Ubuntu 24.04; this decision changes the disposable Sandbox guest only.
- **Reconsider when:** Measured image transfer or cold-start cost dominates Job latency, a bootstrap
  tool has no cross-repository consumer, Debian prevents a real managed-repository terminal, or
  repository preparation repeatedly needs another tool whose inclusion is cheaper and safer than a
  content-addressed setup cache.
