# D037: New core Rooms use a global deployment profile, never a repository contract

- **Applicability:** historical
- **Areas:** core, deployment
- **Read when:** Reviewing the former deployment-profile boundary for caller-managed Rooms.
- **Decision history:** Superseded by D047 — 2026-08-08
- **Decision:** New caller-managed Rooms use one host-local deployment profile under the XDG config
  boundary. The profile selects the built-in Environment configuration and names the default
  Provider Connection; it contains no provider credential, route key, Room lifecycle, or Job state.
  A successful `provider connect` selects that connection for new Rooms. `worker spawn NAME` uses
  the profile with no required option, while an explicit provider override remains available for
  current dogfood and repair. Generic Worker and Job commands never consult `.dorf.toml`;
  repository contracts remain workflow-owned inputs for coding setup, checks, review, and
  publication.
- **Why:** Room creation needs stable host deployment choices, but the caller's current directory is
  neither an authority for a Worker nor a safe source of environment/model behavior. Keeping
  provider credentials in the Provider Gateway, lifecycle in runtime SQLite, and only references in
  the profile preserves existing authorities while making summon repository-neutral.
- **Compatibility:** The profile path and JSON shape are internal and replaceable before the public
  release. Existing recorded Rooms remain self-describing and do not require the profile for
  inspection, messaging, assignment, recovery, or cleanup.
- **Reconsider when:** A second Environment proves a concrete selection seam, multiple validated
  Provider Connections require an interactive default chooser, or a remote/multi-user authority
  makes a per-host XDG profile the wrong ownership boundary.
