# D137: Provider images share one Nix workstation

- **Applicability:** current
- **Areas:** release, sandboxes, harnesses
- **Read when:** Changing shared guest tools, browser installation, or package parity between Incus and E2B.
- **Decision history:** Refines D064 tool packaging, replaces D114's Dorf-managed browser service, and extends D136 to the shared workstation, 2026-09-15.
- **Decision:** Both provider builders install the same immutable Nix workstation closure. The
  profile contains the language runtimes, compilers, utilities, Pi, and browser tools. Codex keeps
  its independently selected runner profile so its existing upgrade operation cannot remove the
  workstation. Debian supplies the provider OS and the minimal Nix bootstrap, rather than a second
  developer-tool installation path.
- **Package authority:** Use the pinned Nixpkgs toolchain and small pinned recipes for the current
  Pi, uv, and browser packages. Pi's dependency lock and browser wheel URLs/hashes retain their
  complete fetch inputs. Package environment provenance and actual versions belong in image
  metadata; provider manifests retain the complete guest recipe input hashes.
- **Browser boundary:** Preinstall Chromium, browser-use, and its unchanged upstream skill.
  Browser-use controls Chromium directly through CDP; Playwright is not part of the package set.
  No Dorf service starts, monitors, reconnects, or stops a browser. The agent starts and manages
  its browser process when needed. Keep any future environment guidance separate from the upstream
  skill, and add it only when actual agent friction establishes the need.
- **Why:** The agent receives a prepared workstation on either provider. Provider-specific browser
  orchestration and separate package installers create differences that do not help the task.
  Mutable browser state belongs to the Sandbox; process continuity is not a package guarantee.
- **Upgrade scope:** Standardizing installation does not add live upgrades for every package.
  The verified retained-task activation and rollback mechanism remains scoped to Codex. A general
  package update framework, automatic rollout, and offline delivery remain deferred.
- **Verification:** Fresh-image probes exercise native compilation, Python environment creation,
  browser-use navigation through an agent-started browser, and retained Codex activation/rollback
  on both providers. Compare the observed workstation store path and inventory for exact parity.
