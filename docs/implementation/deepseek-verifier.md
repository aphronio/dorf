# DeepSeek Diff-Correctness Verifier Role — proposed implementation guide

- **Status:** Proposed implementation guide for the issue #32 shadow slice. This
  document is non-normative: the decision log records only accepted choices, and
  D046 was **not** accepted before the required real broker/Pi/Room terminal.
  Nothing here claims observed live upstream affinity; the real shadow run is the
  first observation point.
- **Role:** `diff` under `[verification.roles]` in `.dorf.toml`, run explicitly via
  `dorf verify-role JOB diff`. It is a shadow/advisory building block, not an
  automatic AFK replacement: the broad Codex reviewer under `[review.agents]` remains
  enabled and this slice does not avoid AFK Codex cost.
- **Calibration:** Linked as the completed blind-calibration stage for diff correctness
  (see the issue's related direction #31); this slice adds the credential-free route,
  Pi session, verdict, evidence, and cleanup terminal on top of that historical
  evaluation. Automatic AFK composition (replacing the broad reviewer) is the next
  step after the real shadow terminal, driven by shadow-run and repair-churn evidence.

## What one role run does

`dorf verify-role JOB diff` runs one bounded shadow or advisory verification attached
to the exact remote head of the coding Job's branch:

1. Resolves the implementation commit (remote `job_branch` head) and clones with one
   narrowly minted installation token: restricted to the exact repository with
   `contents: read` only. The same narrow token is used for branch resolution and the
   clone; the ordinary write-capable implementation token is never used by the
   verifier.
2. Looks up the run key `(JOB, role, commit, config digest, generation)` where the
   config digest is the canonical digest of the complete typed role configuration
   (role name, harness, connection, model, reasoning effort, authority, room policy,
   timeout, prompt). A changed configuration creates a fresh run with a fresh
   deterministic Worker identity instead of reusing or retroactively delivering a
   verdict produced under a different configuration.
3. Spawns one disposable verifier Worker/Room (`provenance=verification`,
   `lifecycle_policy=dedicated`) with a consumer-scoped broker route configured for
   the DeepSeek-prefixed model. The verifier process receives no ChatGPT model
   configuration, implementation conversation, or upstream key. The broker key is
   not a cryptographic per-provider allowlist; the exact boundary is described below.
4. Clones the repository inside the Room at exactly that commit (detached HEAD), with
   a short-lived read credential that is destroyed before the Pi session; no credential
   helper, store file, or embedded-token remote survives.
5. Installs integrity-pinned Node (v22.23.2, the latest official v22 satisfying Pi's
   `>=22.19.0` engine, SHA-256 from nodejs.org `SHASUMS256.txt`) and Pi (0.83.0,
   SHA-512 compared in npm's base64 integrity encoding) into the Room — this first
   slice provisions per-Room instead of expanding the official image. Node is a
   verified archive extraction (one top-level release directory, so extraction
   strips exactly one component). Pi is installed directly from its verified local
   tarball through the pinned Node/npm into an isolated global prefix
   (`/opt/dorf/verifier`): the published package is never unpacked and npm never
   runs from inside it. npm resolves the shrinkwrap and writes the launcher at
   `$prefix/bin/pi` with the package under
   `$prefix/lib/node_modules/@earendil-works/pi-coding-agent`; the route extension
   lives at the stable private path `$prefix/extensions/` outside the package.
6. Renders the review diff (`target_start_sha..commit`) outside the clone and writes the
   pinned review protocol into the Room.
7. Runs a fresh Pi session: `-p`, `--provider dorf-deepseek`,
   `--model dorf-deepseek/deepseek/deepseek-v4-flash` (prefix pinning guarantees this
   invocation and role can only reference the intended DeepSeek Pi model id and
   tools; exact live upstream affinity is only established by the real shadow
   terminal), `--thinking max` (or the typed reasoning effort),
   `--tools read,grep,find,ls`, `--no-session`, `--no-approve`, `--no-context-files`,
   bounded by the typed timeout. There is no Codex authentication probe: that probe
   is implementation-agent-specific and may touch a ChatGPT model through the
   broker; route/provider failure is classified from the exact prefixed Pi
   invocation itself.
8. Records before/after HEAD, tree, and worktree observations; any change invalidates
   the result (`infrastructure / commit_changed`).
9. Records exactly one verdict — `findings`, `no-findings`, or `infrastructure` — as a
   workflow fact timeline event plus a command-run artifact, then cleans up: route
   revocation, verifier Room destruction (which removes the temporary clone), each
   retry-safe and visibly pending until reconciled. The verdict event is appended
   idempotently during terminal reconciliation, so a crash between storing the verdict
   and appending the event is healed on the next invocation.

## Outcome semantics

- **Findings from an `advisory` run** are delivered exactly once through the original
  implementation Job FIFO as an advisory input (deterministic
  `verifier:JOB:role:commit:run` action identity, so retries cannot duplicate). The
  packet is bounded to keep one runaway review from flooding the FIFO; the complete
  output stays retained as the run artifact. Findings never create a second repair
  Job, branch, or PR and never authorize merge.
- **Findings from a `shadow` run** are retained as verifier evidence exactly like a
  no-findings verdict: the verdict, command-run artifact, and timeline event persist
  and the disposable Room/route are cleaned, but nothing is ever enqueued to the
  implementation Job FIFO. CLI messages and the verdict event state the authority
  explicitly (for example "shadow findings retained as verifier evidence; not
  delivered to the Job FIFO").
- **No-findings** requires the complete non-empty Pi response to be exactly the
  sentinel `DORF_REVIEW_NO_FINDINGS`; findings text followed by the sentinel is still
  findings. It is retained evidence only and does not independently authorize merge.
- **Infrastructure** outcomes (provider/route failure observed by the exact prefixed
  Pi invocation, Pi process, timeout, clone, diff render, cleanup, lost Room,
  superseded configuration) are recorded distinctly and never become code findings.
- Repeated coordination is idempotent per `(JOB, role, commit, config digest,
  generation)`: a terminal verdict is re-reported without a new Room, route, run, or
  feedback. A crashed run with a surviving Room resumes the same run — including a
  crash after Worker creation but before the Room identity is recorded, which
  recovers the deterministic Worker instead of spawning a duplicate. A terminal
  infrastructure failure (for example a lost Room) starts a new generation on the
  next invocation. A configuration change supersedes any still-running run for the
  commit (recorded as `infrastructure / config_changed` and cleaned) before the
  fresh run starts.

## Configuration surface

`[verification.roles.NAME]` in `.dorf.toml` is typed and deliberately has **no
`command` field** — repositories cannot declare arbitrary host commands for a role:

```toml
[verification.roles.diff]
harness = "pi"            # only the Pi harness is supported
connection = "deepseek"          # reusable named connection, never the profile default
model = "deepseek-v4-flash"   # upstream model id; the broker prefix is applied automatically
reasoning_effort = "max"  # Pi reasoning levels only: minimal, low, medium, high, xhigh, max
authority = "shadow"     # shadow (retain evidence, never deliver) or advisory (deliver findings exactly once)
room = "dedicated"        # dedicated disposable Room per run
timeout_seconds = 1800    # bounded Pi execution
prompt = "..."            # pinned review protocol (job placeholders supported)
```

Reasoning levels are validated against Pi's thinking levels (`minimal`, `low`,
`medium`, `high`, `xhigh`, `max`), not Codex's, so `ultra` is rejected for verifier
roles. Authority accepts exactly `shadow` or `advisory`: a shadow run may persist
verdict/evidence and clean resources but never enqueues findings; an advisory run may
enqueue findings exactly once as designed. Neither has merge authority. The broad
Codex reviewer remains configured under `[review.agents]` for explicit targeted
adjudication and is unaffected; the typed role is opt-in via the command.

## Run identity and provenance

Each run row persists the canonical configuration snapshot (JSON covering role name,
harness, connection, model, reasoning effort, authority, room policy, timeout, and
full prompt) plus its `sha256:` digest. Runs are only reused or re-reported for a
commit when the configuration digest matches; changing shadow to advisory, the model,
reasoning effort, provider connection, prompt, or any other typed field creates a
fresh run. The configuration digest is part of the deterministic verifier Worker
identity, so a changed configuration cannot collide with the ended Worker from an
earlier configuration. The verdict timeline event carries the authority, config
digest, typed config fields, and a prompt digest. The experimental `verifier_runs`
table has no compatibility burden: a table lacking the typed configuration columns
fails clearly at open instead of being silently misread.

## Provider gateway

- `dorf provider connect deepseek --api-key --name NAME` reads the key from
  `DEEPSEEK_API_KEY` or a hidden prompt, stores it in protected host state
  (`credentials/` under the 0700 gateway state dir), and **keeps the deployment
  profile default untouched** unless `--set-default` is passed explicitly.
- The broker config gains a `codex-api-key` entry with `base-url
  https://api.deepseek.com/v1` and `prefix: deepseek`. A request for
  `deepseek/<model>` is designed to route to that entry only with the prefix stripped
  upstream; unprefixed models (for example the ChatGPT implementation route) cannot
  silently pool or fall back onto DeepSeek and vice versa. Prefix pinning guarantees
  the Pi invocation selects the intended DeepSeek Pi model/tools and the role exposes
  only the configured DeepSeek Pi model; CLIProxyAPI keys are broker keys, not
  cryptographic per-provider allowlists, so the route credential itself is not
  claimed to be provider-exclusive. Exact live upstream affinity is not claimed until
  the real broker/Pi/Room terminal observes it.
- Route creation fails closed unless at most one connection is unprefixed and every
  prefixed connection has a distinct prefix (two DeepSeek connections, or a second
  unprefixed connection, are rejected).
- Only the broker-local route credential (`agw_…`) enters the verifier Room; the
  verifier process never receives a ChatGPT model configuration and never calls the
  Codex authentication probe.

## Remaining work for the real credential-free terminal

1. One real shadow run against an exact coding commit with a live DeepSeek connection:
   `dorf provider connect deepseek --api-key --name deepseek` (host-side), then
   `dorf verify-role JOB diff`; confirm the Pi turn, verdict, evidence, absence of
   FIFO delivery in shadow mode, route revocation, Room absence, and clone cleanup.
2. Confirm the DeepSeek upstream model id `deepseek-v4-flash` and its Responses wire
   dialect through the pinned broker (the typed `model` may need adjusting; the Pi
   extension speaks `openai-responses` to the broker's Responses route).
3. Promote the pinned Node/Pi toolchain into the official image only if repeated
   per-Room installs measurably dominate Room cost (the image schema stays untouched
   for now).
4. After the real shadow terminal, compose the role automatically into the coding
   workflow (the AFK replacement step); until then the role stays an explicit
   shadow/advisory command and no AFK Codex cost is avoided.
