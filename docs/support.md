# Setup support

`dorf setup` and `dorf doctor` are the setup support interface. Run the reported command,
follow its single safe action, and rerun setup. Do not look for a matching error page: the CLI
derives remediation from the machine state it just observed.

When a command pauses or fails, it writes:

- `diagnostic.md` for a person;
- `diagnostic.json` for a coding agent or other tool; and
- `commands.log` for bounded command evidence when available.

These files live under `$XDG_STATE_HOME/dorf/diagnostics/`, or
`~/.local/state/dorf/diagnostics/` when `XDG_STATE_HOME` is unset. They use private directory
and file permissions. Credential-shaped values are redacted, but review the files before sharing
them because host facts can still be identifying.

## Supported host boundary

The first reviewed host-convergence recipe is x86_64 Arch Linux with hardware virtualization and
`/dev/kvm`. It installs the distribution's Incus package, starts the local service, and initializes
local storage plus a private NAT network.

Other x86_64 Linux hosts may work when Incus is already installed and usable, but Dorf does not
yet change their packages, services, or groups. macOS, Windows, other architectures, and remote
Incus daemons are not supported local-Room hosts in the initial release.

Membership in `incus-admin` grants root-equivalent control of the machine through Incus. Setup
explains and asks before adding a user to that group or making another administrator-level change.
The reviewed local initialization does not enable Incus's remote API.

## Ownership

Use the diagnostic's `owner` and `classification` before deciding where a problem belongs:

| Owner | Boundary |
| --- | --- |
| `host` | Firmware virtualization, KVM availability, memory, disk, or an unsupported host |
| `packaging` | The supported distribution package or service operation |
| `incus` | A minimal Incus operation, guest agent, private network, or VM lifecycle |
| `dorf` | Convergence, recorded state, cleanup, diagnostics, or secret handling |
| `codex` | Codex app-server behavior in the current official clean image |
| `provider-gateway` | Provider connection, broker health, scoped route, or upstream authentication |

A likely upstream classification is evidence to isolate the failing boundary, not permission to
open an issue automatically. Before reporting, reproduce the smallest operation outside Dorf
when possible, confirm no disposable Room or provider route remains, and include only reviewed
diagnostic facts. A human chooses whether and where to submit the report.

Coding agents use the same commands and the agent-readable JSON. They should apply listed safe
actions only within their existing authority and ask before any action listed as requiring human
approval. Dorf does not emit an agent prompt or grant an agent additional permission.
