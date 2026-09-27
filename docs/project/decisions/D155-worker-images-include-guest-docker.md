# D155: Worker images include guest Docker

- **Applicability:** current
- **Areas:** release, sandboxes
- **Read when:** Changing guest container tooling, daemon startup, or provider image parity.
- **Decision history:** Accepted 2026-09-27; refines D137's Debian package boundary.
- **Decision:** The shared Debian guest recipe installs Docker Engine, CLI, Compose and Buildx.
  Debian owns these OS services and their systemd units; the Nix workstation continues to own
  developer tools. Enable the guest Docker and containerd services for VM boot. Image metadata
  records the installed Debian package versions alongside the existing immutable Nix provenance.
- **Why:** Real project startup needs Compose. Installing the same root-level prerequisites in
  every task repeats work and makes readiness depend on package mirrors. One provider-neutral
  image recipe supplies the capability without another worker type or application startup recipe.
- **Boundary:** Containers run inside the isolated guest VM. Never mount the provider host's Docker
  socket. Repositories own their Compose applications, fixtures, credentials and cleanup. No
  application source or application container images are baked into the standard worker image.
- **Cost:** The base image grows and runs a guest daemon. Measure startup cost before introducing
  another image variant or custom daemon lifecycle. Existing VMs retain their admitted contents;
  this adds no live package migration.
- **Verification:** Fresh instances on both providers must build and serve a healthy Compose
  application without installing packages or manually starting Docker. The existing workstation
  probe also checks package metadata and removes its Compose resources.
