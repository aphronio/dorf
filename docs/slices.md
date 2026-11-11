# Slices

Each slice is the smallest demoable step. A slice is done when its proof runs from one command.

| Slice | Delivers | Proof | Status |
| --- | --- | --- | --- |
| S0 | New North Star, repository rules and README; removal of the 0.31 code and docs | Secret scan green on the reduced repository | Done |
| S1 | One Thread, one Sandbox, pause and resume on Agent Substrate | `deploy/proof.sh substrate` and `deploy/proof.sh sandbox-lifecycle` on a cluster running Substrate, with the faux model: tool call writes a file and starts a process, and the Sandbox reaches the internet but not the cluster; the Sandbox pauses after the run; the next message resumes it with both intact; an agent killed mid-call finishes the call after restart, running it once; a timeout kills its command; archiving the Thread mid-call stops it and pauses the Sandbox at once, and a message brings it back; deleting the Thread deletes the Sandbox. Then one manual run with a real model | Done |
| S2 | Harmless Substrate upgrades | `bun run --cwd apps/agent proof:upgrade` on a fresh machine: installs the previous Substrate release on kind, then upgrades to the pinned one and rolls back with no hand steps while two Threads run tool calls and a third is paused. No tool call fails, no Sandbox crashes, busy Sandboxes move workers, and every Thread keeps its files and processes | Done |
| S3 | A real repository, used daily | `bun run --cwd apps/agent proof:repository` with `DORF_PROOF_REPO` set to a private repository with a `node --test` suite: a Thread clones it, changes it, runs the tests, commits and pushes a branch the remote then shows, with the token injected by the egress gateway. Then one run with a real model on this repository | Done |
| S3.5 | The agent reaches Sandboxes without `kubectl` or a cluster kubeconfig, through Dorf's gate, so it can later run on Cloudflare | `deploy/proof.sh sandbox-lifecycle`: the agent holds only a `dorf-agent` token and calls the gate over HTTP and WebSocket; every step of S1's proof still passes | Done |
| S4 | Dogfood: build Dorf with Dorf | From a minimal chat page, start a Thread on this repository, follow its output live, reply, and get a pushed branch. The agent runs as a pod beside the gate and Substrate, on a service-account token Kubernetes renews, with Claude connected from the chat page; the chat page is reached only over Tailscale. Sandboxes commit with a default git identity. Friction found while using it is filed as `friction` issues, and those issues pick the next slice | Planned |
| S4.1 | Pick the model and thinking per Thread, and persistent subagents (owner asked while dogfooding) | `bun run --cwd apps/agent proof:models` with the faux model: the default model leads the offered list with its thinking levels; a Thread runs on the model and level it was created with, the level changes mid-Thread, and unknown models or levels are refused; a subagent works in the Thread's Sandbox, keeps its conversation across messages and returns each answer; a working subagent keeps the Sandbox awake; stop and Thread deletion end its work. Then `proof:claude-plan`, which covers the plan's model list, a live thinking change and a steer. It needs Claude Code signed in on the machine it runs on | Built; the plan checks ran against the lab agent; the steer fix awaits a run |
| S4.2 | A long Thread keeps its whole history on the page (found reviewing Dorf against Pi's examples) | `bun run --cwd apps/agent proof:history` with the faux model: a Thread of about 130 entries streams its newest fifty and says earlier ones exist; loading earlier pages the rest, in order, back to its first message; a reconnect that missed 10 or nearly all entries gets exactly those | Built |
| S4.3 | A Thread proves its own change to Dorf, with no cluster and no credentials (found reviewing Dorf for agents) | `bun run --cwd apps/agent proof` inside a Sandbox: history, models, lifecycle, make-room and images pass against local stand-ins of the gate, each Sandbox a folder of its own on one machine, and each proof says so on its first line. The same files pass through `deploy/proof.sh` on Substrate, where `deploy/proof.sh substrate` adds what only Substrate does: a file and a process survive a pause, Sandboxes don't see each other's files, the internet is reachable and the cluster is not | Built |
| S4.4 | A Thread follows the repository's AGENTS.md and skills (found when a fresh Thread on Dorf never read AGENTS.md) | `bun run --cwd apps/agent proof:models` with the faux model: after a Thread writes an AGENTS.md and a skill in its Sandbox, its next request carries the instructions and lists the skill with its description and location | Built |
| S4.5 | Images in a message: paste, drop or attach them when starting a Thread, sending, or steering a run (owner asked) | `bun run --cwd apps/agent proof:images` with the faux model: a Thread starts from text and an image and the model sees it fitted to its limits; a message of only images carries no empty text; an image sent mid-run lands after the running tool call; `read` fits a wide image the same way and refuses one whose header claims more pixels than the bound, before decoding it; every image survives an agent restart; bad, corrupt, too many or too large images get a plain error and create no Thread | Built |
| S4.6 | Repository snapshots: a new Thread on a repository starts from a saved Sandbox that already holds the clone with its `.agents/setup` run, as Amp's orbs do (owner asked) | `bun run --cwd apps/agent proof:repo-snapshots` with the faux model against the stand-ins: a second Thread starts from the snapshot and its setup installs nothing; a commit that makes catch-up slow builds a newer snapshot in the background, one that keeps it fast does not; an agent killed mid-build finishes it; a refused snapshot falls back to a plain Sandbox and starts a rebuild; a failed setup saves nothing. Then `deploy/proof.sh snapshots`: a tag holds files and processes; a fork borrows its tag, which the reaper spares until the fork's first sync pause; a newer template refuses an older tag with 412 | Built; the Substrate proof awaits a run |
| S5 | Execution nodes anywhere | The lab moves from kind to k3s meshed over Tailscale (Headscale for self-hosters); a machine on another network joins as a node, a Thread's Sandbox runs there, and its tool-call latency is measured against a local Sandbox | Planned after S4 |
| S6 | GitHub triggers | A mention or failing check starts or continues a Thread | Deferred until dogfooding asks for it |
| S7 | Agent off-site | The agent runs outside the cluster: the gate is exposed and the agent proves its identity from outside | Deferred until a host cannot join the cluster |

## Carry-over

Facts learned in one slice that a later slice must respect.

- Repository snapshots (`apps/agent/src/repo-snapshots.ts`). The session doc `dorf.repo-snapshots`
  holds, per repository, the snapshot new Threads start from, its commit, and the build task running.
  A Thread's start fetches and resets the clone to the default branch's tip and runs setup again, so a
  stale snapshot costs only the catch-up. A build starts, in the commit that submits the first
  message, when the Thread did not start from the snapshot, or caught up from another commit in more
  than `DORF_SLOW_CATCH_UP_MS` (10 s). The build reaps first, never last, then sets up a builder
  Sandbox `g-<repo hash>` that never makes room, pauses it sync and tags it.
- A Substrate tag is a snapshot. CreateTag is synchronous and not idempotent, so a cut-off try leaves
  a tag that is not ready and goes first. A Sandbox created from a tag has only the tag's snapshot
  until its first pause finishes uploading; deleting the tag before then loses it. The gate reports a
  tag borrowed while some Sandbox's newest finished snapshot is the tag's; matching any listed
  snapshot would overcount, since the entry lingers. Substrate refuses a tag from another template or
  being deleted with FailedPrecondition, which the gate answers 412, and the agent then creates a
  plain Sandbox.

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
- `claude-plan/<model>` serves Claude from the owner's Claude plan through the unmodified Claude Code
  binary, signed in on the agent's machine; tools still run in the Sandbox. Claude Code keeps its own
  copy of each Thread and gets only what it has not seen; a Pi compaction starts a new copy. Claude
  Code reports extra usage only with the response that used it, so extra usage stays off in claude.ai.
  Proof: `bun run --cwd apps/agent proof:claude-plan`. It spends the owner's plan and reads as automation, so run it
  only when `claude-plan.ts`, `claude-session.ts` or the Agent SDK pin changes; every other proof
  uses the faux model.
- The plan's models and their effort levels come from Claude Code's start-up handshake, which sends
  no model request. Pi's `fetchModels` loads them when the agent starts, before Threads resume, and
  after the owner signs in; only the default model's provider is offered. Pi's thinking levels are
  Claude Code's effort levels, and a change reaches the running Claude Code session through
  `applyFlagSettings`, so it is not relaunched. A message sent mid-run steers: Pi places it after
  the tool round, and the provider queues it into Claude Code's running turn with `priority:
  "next"`, so the model reads it as the user's. Put inside a tool result, the model treated it as
  prompt injection and refused it.
- Subagents keep Pi's example 22 anchor: each is a conversation owned by a background anchor task,
  pointed at the Thread's Sandbox. A `subagent` call steers its message to the subagent and returns
  the answer as its result, as Pi's coding agent does; calls in one round run at once. The owner's
  message ends a wait, and that answer then reaches the Thread once as a steered
  `dorf.subagent-report` entry, so the page knows its author without reading the text. Nothing is a
  follow-up. Pi Durable 1.5.0 has no `notify` or parked tools, so a call waits in memory and the
  agent's restart replays it by request ID; park instead once a release has them. The idle pause
  skips a Thread while one of its subagents works. Deleting a Thread aborts with
  `background: true`, which stops its subagents. A subagent runs on the Thread's model and thinking
  level unless its first call names another model.
  Pi 1.5.0's example returns `content` from the tool, but tools must return `output`.
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
  as root and owns apt's directories. The image carries Node and Bun, Dorf's own toolchain, and
  Chromium with the Python that Browser Harness runs on; the agent installs any other toolchain a
  repository needs.
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
- A gVisor worker hosts as many Sandboxes as its memory limit books, less a 64 MiB reserve
  (Substrate's `cmd/ateworker-gvisor/memory.go`). The pool runs one worker of 12 CPU and 25 GiB,
  so its limits book six Sandboxes of 2 CPU and 4 GiB at once; Dorf's heaviest work (`bun run proof`, a page
  build, Chromium) peaked at 2.1 GiB in one, measured from inside, and ran 8% slower than at 4 CPU.
  A seventh resume fails with `no worker has room for the actor`, and the agent makes room
  (`bun run --cwd apps/agent proof:make-room`). An async pause frees its booking before it
  returns, so the retry needs no wait.
- The chat page is reached through `tailscale serve --bg --https=443 http://127.0.0.1:8091` on the
  cluster's machine, in front of a systemd unit running `kubectl -n dorf-gate port-forward
  deploy/dorf-agent 8091:8091` with `Restart=always`: the forward exits whenever `up.sh` replaces the
  pod. `up.sh` needs `DORF_OWNER`, the one Tailscale login the agent serves.
- GitHub is on when the Secret `dorf-secrets/github` exists: `up.sh` then gives the gate the egress
  rule for github.com, api.github.com and uploads.github.com, and the agent its own copy to list
  repositories. The gateway swaps the whole Authorization value, so `gh` works with
  `GH_TOKEN=placeholder` on REST and GraphQL. A fine-grained token lists every public repository of
  its owner too, not only the granted ones.
- A Sandbox has no user or mount namespaces (gVisor), so local stand-ins can't give each Sandbox
  its own filesystem. Each gets its own folder instead: the stand-in starts that Sandbox's pi-env
  there and the workspace is `/proc/self/cwd`. Pi resolves a relative workspace on the agent's side,
  so `.` does not work. Absolute paths and processes stay shared; isolation is a Substrate proof.
  The agent keeps its command jobs in `DORF_JOBS` (default `/var/tmp/dorf-jobs`): job ids restart
  with each agent's database, so two agents on one machine, such as a proof in a Thread's Sandbox,
  must not share it.
- Proofs run the working tree's agent against what the cluster already runs; they do not deploy,
  since `up.sh` rolls the agent the owner is using. Run `deploy/up.sh` first when the gate, the
  Sandbox image or `deploy/` changed; `deploy/proof.sh <name>` runs a proof file there.
  `proof:upgrade` still deploys: a fresh install is its subject.
- `deploy/up.sh` gives the pool the worker image of the installed Substrate release, so only
  `deploy/substrate.sh` moves workers between releases.
- Substrate's API speaks only gRPC: plain HTTP/1.1 does not connect and JSON gets 415, so a Worker
  cannot call it. The gate (`apps/gate`) is the agent's only door: HTTP for create, state, pause
  and delete, and a WebSocket per pi-env connection. A design where the agent called Substrate's
  API itself (with a door only for bytes) was as fast but cannot run in a Worker, and its token
  also had to work at the Kubernetes API.
- Substrate scopes a ServiceAccount token to one atespace through that atespace's access policy
  (`editor`); the token's audience must be `api.ate-system.svc`. The `dorf-agent` token is denied
  other atespaces, global calls and changing its own policy. The gate forwards it on every call and
  holds no rights over Sandboxes itself.
- Substrate's router accepts clients by an exact SPIFFE allow-list and checks no atespace, so the
  gate's pod certificate could reach every Sandbox. The gate therefore opens a WebSocket only after
  `ResumeActor` succeeds with the caller's token (it needs `editor` and is harmless on a running
  Sandbox). `deploy/substrate.sh` puts `dorf-gate` on the allow-list at install; the install record
  keeps it across upgrades. The gate runs in its own namespace so that identity stays apart from the
  Sandboxes' pool, and has only a ClusterIP: anything outside the cluster reaches it through a port
  forward or a private tunnel, never a node port.
- Substrate has no proxy of its own that accepts a token: the router takes only allow-listed pod
  certificates (token support is a TODO in its client). That is why Dorf runs the gate's byte path;
  replace it once the router accepts tokens scoped by atespace.
- The gate checks the caller once per WebSocket, so a stream outlives its token's expiry or removal
  from the access policy until it closes.
- Before the agent can run in a Worker: pi-env's client still spawns a process (`pipe.ts`), the
  agent reads its token from a file, and ServiceAccount tokens expire, so something with the right
  to create tokens must renew an off-cluster agent's token.
- Through the gate, resuming to the first byte takes about 195 ms, down from about 816 ms through
  a `kubectl ate proxy` per connection; warm calls and a 100 MB stream are unchanged. A call in
  flight survives the gate's pod being deleted: pi-env reconnects and the job reattaches.
