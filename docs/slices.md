# Slices

Each slice is the smallest demoable step. A slice is done when its proof runs from one command.

| Slice | Delivers | Proof | Status |
| --- | --- | --- | --- |
| S0 | New North Star, repository rules and README; removal of the 0.31 code and docs | Secret scan green on the reduced repository | Done |
| S1 | One Thread, one Sandbox, pause and resume on Agent Substrate | `npm --prefix apps/agent run proof` on a cluster running Substrate, with the faux model: tool call writes a file and starts a process; the Sandbox pauses after the run; the next message resumes it with both intact; an agent killed mid-call finishes after restart; deleting the Thread deletes the Sandbox. Then one manual run with a real model | Done |
| S2 | Harmless Substrate upgrades | Scripted upgrade and rollback with drain, zero hand steps, under a live Thread, in CI | Planned |
| S3 | A real repository, used daily | Clone, change, test and push from a Thread | Planned |
| S4 | GitHub triggers | A mention or failing check starts or continues a Thread | Planned |

## Carry-over

Facts learned in one slice that a later slice must respect.

- Substrate's router carries only HTTP. pi-env speaks a framed protocol over stdio, so the Sandbox
  init bridges one upgraded HTTP request to the pi-env daemon.
- The Sandbox's main process must ignore `SIGTERM`; Substrate signals it when its worker goes away,
  and a process that exits cannot be paused.
- Substrate does not pause a Sandbox when its worker terminates. The upgrade script must drain
  first, or awake Sandboxes crash.
- A pi-env connection that lived through a pause fails its next request. Close it before pausing so
  the client reconnects cleanly.
- The pi-env daemon and the init are frozen into each Sandbox's memory snapshot at creation. Keep
  them small and stable.
- The router passes only `Upgrade: websocket` requests; any other upgrade gets 403.
- Sandbox image builds are not reproducible, so `deploy/up.sh` tags the image with a hash of its
  inputs and builds only when the template lacks that tag. Replacing a template (new image for new
  Threads) is not supported yet.
- Substrate denies all egress by default. Each Sandbox gets a policy at create that allows every
  host, with TLS passed through untouched. An `https` rule would instead intercept TLS, which needs
  Substrate's trust bundle in the image.
- Root in a Sandbox has only three capabilities (no setuid, no DAC override), so the image runs apt
  as root and owns apt's directories. The image carries no language toolchains; the agent installs
  what a repository needs.
- Substrate's egress gateway can inject a credential into requests to a named host, from a
  Kubernetes Secret. That lets a Sandbox push to a repository without holding the token (S3).
