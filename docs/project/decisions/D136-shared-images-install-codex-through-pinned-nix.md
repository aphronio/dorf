# D136: Shared images install Codex through pinned Nix

- **Applicability:** partial
- **Areas:** release, harnesses, sandboxes
- **Read when:** Building guest images or changing how persistent Sandboxes obtain Codex packages.
- **Decision history:** Refines D066's Codex npm installation; composes with D135 package recovery, 2026-09-15; extended to the shared workstation by D137, 2026-09-15.
- **Decision:** The shared Debian guest recipe installs pinned Nix and an initial Codex generation.
  Nix packages the official prebuilt executable from a hash-pinned archive. Pi retains its existing
  npm installation. Both Incus and E2B builders consume the same package inputs and retain their
  provenance. The release manifest retains the Nix manager, source archive, and immutable store path.
- **Why:** Persistent conversations need a small executable update mechanism without moving their
  writable state to a fresh base image. One package recipe shared by image construction and upgrade
  proofs avoids parallel installation paths and conflicting version pins.
- **Scope:** Download and verify pinned packages directly using the guest's existing Internet access
  before holding delivery. The existing retained-task coordinator activates the staged closure with
  checkpoint recovery. No general package distribution service or offline transport is introduced.
  A blocked download fails staging without changing network policy or the active executable.
- **State boundary:** Nix generations select executables; provider checkpoints recover mutable
  conversation and workspace data. Updating an image or profile affects future VMs. Existing VMs
  need a separate one-time Nix bootstrap. Live-process continuity is not guaranteed by a filesystem
  snapshot; Incus recovery includes a VM stop and restart.
- **Verification:** Disposable candidates exercise their baked-in Nix installation, direct package
  staging, exact native conversation continuation, rollback, and resource cleanup through the
  retained-worker recipe on both providers. Local fixtures do not prove real Provider Gateway
  reconnection. The active plan records the completed evidence and remaining scope.
