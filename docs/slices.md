# Slices

Each slice is the smallest demoable step. A slice is done when its proof runs from one command.

| Slice | Delivers | Proof | Status |
| --- | --- | --- | --- |
| S0 | New North Star, repository rules and README; removal of the 0.31 code and docs | Secret scan green on the reduced repository | Done |
| S1 | One Thread, one Sandbox, pause and resume on Agent Substrate | `npm --prefix apps/agent run proof:lifecycle` on a cluster running Substrate, with the faux model: tool call writes a file and starts a process, and the Sandbox reaches the internet but not the cluster; the Sandbox pauses after the run; the next message resumes it with both intact; an agent killed mid-call finishes the call after restart, running it once; a timeout kills its command; deleting the Thread deletes the Sandbox. Then one manual run with a real model | Done |
| S2 | Harmless Substrate upgrades | `npm --prefix apps/agent run proof:upgrade` on a fresh machine: installs the previous Substrate release on kind, then upgrades to the pinned one and rolls back with no hand steps while two Threads run tool calls and a third is paused. No tool call fails, no Sandbox crashes, busy Sandboxes move workers, and every Thread keeps its files and processes | Done |
| S3 | A real repository, used daily | `npm --prefix apps/agent run proof:repository` with `DORF_PROOF_REPO` set to a private repository with a `node --test` suite: a Thread clones it, changes it, runs the tests, commits and pushes a branch the remote then shows, with the token injected by the egress gateway. Then daily use with a real model on this repository | In progress |
| S4 | GitHub triggers | A mention or failing check starts or continues a Thread | Planned |

## Carry-over

Facts learned in one slice that a later slice must respect.

- Substrate's router carries only HTTP and WebSocket. pi-env speaks a framed protocol over stdio,
  so the Sandbox init turns each WebSocket on `/pienv` into a fresh pi-env daemon, its bytes in
  binary messages.
- The Sandbox's main process must ignore `SIGTERM`; Substrate signals it when its worker goes away,
  and a process that exits cannot be paused.
- Substrate does not pause a Sandbox when its worker terminates; it signals the Sandbox and allows
  30 minutes. The agent drains: it polls for worker pods being deleted and pauses their running
  Sandboxes with `--mode=sync`, mid-call if need be. An async pause can lose its snapshot if the
  node goes away, so idle pauses (async) are at risk only on node loss.
- A pi-env daemon kills the commands it started when its stream ends, and Substrate's router cuts
  open streams about 28 s after a router pod starts terminating (13 s drain delay, then Envoy's
  drain), on every router roll. Bash calls therefore run as Sandbox jobs that reattach
  (`apps/agent/src/jobs.ts`, a wrapper around Pi's bash). pi-env's `exec` does not say which
  failures were a lost connection (earendil-works/pi#11976), so the wrapper retries every failure
  to reach the daemon except a deleted Sandbox.
- Known limits of jobs: `read`, `write` and `edit` calls in flight during a drain or router roll
  still fail once. A call cut off by more agent restarts than Pi's `restartLimit` (5) ends while
  its job runs on. A timeout restarts its clock after an agent restart. Output that is not UTF-8
  can be skipped or repeated at a reattach, and Pi's "Full output" file then holds only the part
  after it.
- A connection kept through an idle pause makes the next call wait until the old daemon exits,
  about 3 s. An idle pause closes the stream first; a drain keeps it so calls in flight reattach.
- `init.mts` as PID 1 never reaped orphans, so finished background commands stayed zombies. The
  image runs `tini` as PID 1.
- The init is frozen into each Sandbox's memory snapshot, and into every tag made from it, so its
  contract (`/pienv`, the `X-Pienv-Args` header) must stay stable. The daemon is not: the init
  starts it from disk for each connection.
- The router passes only `Upgrade: websocket` requests; any other upgrade gets 403.
- Sandbox image builds are not reproducible, so `deploy/up.sh` names the image and its template
  after a hash of their inputs. A Sandbox cannot resume once its template is deleted, so each build
  gets a new `sandbox-<hash>` template, the agent creates Sandboxes from the newest, and `up.sh`
  deletes only templates no Sandbox uses.
- Substrate denies all egress by default. Each Sandbox gets a policy at create that allows every
  host, with TLS passed through untouched. An `https` rule intercepts TLS instead; the template
  mounts Substrate's trust bundle and the image points `SSL_CERT_FILE`, `NODE_EXTRA_CA_CERTS`,
  `GIT_SSL_CAINFO` and `REQUESTS_CA_BUNDLE` at it.
- Root in a Sandbox has only three capabilities (no setuid, no DAC override), so the image runs apt
  as root and owns apt's directories. The image carries no language toolchains; the agent installs
  what a repository needs.
- Substrate's egress gateway injects the GitHub token from Secret `dorf-secrets/github`, so a
  Sandbox pushes without holding it. The gateway only replaces an Authorization header the request
  already carries, so git's credential helper sends a placeholder. Without the Secret, public
  repositories still work and only authenticated requests get 403. The Secret's key `basic` holds
  base64 of `x-access-token:<token>`.
- Egress rules are allow-only host names, and the gateway resolves cluster names too, so an open
  policy reached the Kubernetes API by its cluster name. `deploy/egress-fence.yaml` limits the
  gateway to public addresses, DNS and the two Substrate services it calls; it needs a CNI that
  enforces NetworkPolicy (kind's does). It cannot fence the gateway's own loopback, so keep the
  `http` rule on port 80 (agent-substrate/substrate#2086, #1736).
- Worker capacity is the pool's resource limits. A 4 CPU, 4 GiB worker holds about four 1 CPU,
  1 GiB Sandboxes; at 2 CPU, 1.5 GiB it held one.
- `deploy/up.sh` gives the pool the worker image of the installed Substrate release, so only
  `deploy/substrate.sh` moves workers between releases.
