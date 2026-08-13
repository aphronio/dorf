# E2B Capability Proof

Observed on 2026-08-14 with the E2B Hobby account and JavaScript SDK 2.39.0. This is ecosystem
research, not a supported Dorf profile or the D063 second-provider terminal. The ranked candidate
list remains the [Sandbox and VM Provider Watchlist](sandbox-vm-watchlist.md).

## Result

One 4-vCPU, 4-GB Ubuntu 24.04 template and one disposable Sandbox completed every bounded check:

- Linux x86_64 with `systemd` as PID 1 and passwordless root;
- a 12-GB root disk with about 10 GB free after installing the proof toolchain;
- native files and exec with stdin, separate stdout and stderr, and an exact nonzero exit status;
- a real Docker daemon and successful Docker Compose workload;
- native headless Google Chrome fetching and rendering an external page;
- an HTTPS service whose anonymous request returned 403 and scoped-token request returned 200;
- exactly one Sandbox found by the Dorf purpose and ownership nonce, then reconnected by ID;
- full-memory pause/resume preserving the filesystem and a running background process;
- provider-enforced deny-all outbound networking while the control channel remained usable; and
- terminal kill followed by a metadata query confirming no owned Sandbox remained.

The reusable template is named `dorf-capability-20260814-v1`. The disposable Sandbox was confirmed
absent. Detailed machine-readable evidence is retained locally under the ignored path
`.dorf/e2b-spike/evidence.json`; no API key, Provider Route, or upstream credential entered the
template, Sandbox, evidence, or logs.

## What remains unproved

- The proof reconnected through metadata after deliberately discarding the original handle; it did
  not inject an actual lost create response. The real adapter must fault-inject that ambiguity.
- E2B has official JavaScript and Python SDKs but no official Go SDK. Its official
  [OpenAPI specification](https://github.com/e2b-dev/docs/blob/main/openapi-public.yml) covers the
  platform and in-Sandbox `envd` APIs, so a native generated Go client is feasible but unproved.
- The template did not contain the pinned combined Codex/Pi Harness profile or run a Dorf Job.
- The current Provider Gateway binds plain HTTP to a private Incus bridge. No secure remote path from
  E2B to that owner-hosted authority has been designed or tested. This is the principal blocker to
  a real Codex-on-E2B terminal.
- The Hobby one-hour continuous runtime, sustained cost, regional behavior, service reliability,
  and the new custom runtime's detailed isolation guarantees were not evaluated.

Do not call E2B a supported provider until a real adapter preserves Dorf-owned Action settlement,
route creation and revocation, AgentRun recovery, repository evidence, publication, and confirmed
Job cleanup while satisfying D063's import and branch oracle.
