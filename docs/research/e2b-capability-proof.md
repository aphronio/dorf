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

A second live proof used Dorf's narrow native Go lifecycle client rather than the JavaScript SDK. It
allowed E2B to accept one create request, consumed and deliberately discarded the successful HTTP
response before Dorf could observe the provider ID, rediscovered exactly one running Sandbox by its
Job, Dorf Sandbox ID, and ownership nonce, re-attested that metadata through the individual resource
endpoint, deleted it, and confirmed the ownership query was empty. The first run also caught that
E2B's multi-state list filter is comma-delimited; after correcting that wire detail, the fault-injected
proof passed and a separate inventory confirmed that no lifecycle-proof Sandbox remained.

The reusable template is named `dorf-capability-20260814-v1`. The disposable Sandbox was confirmed
absent. Detailed machine-readable evidence is retained locally under the ignored path
`.dorf/e2b-spike/evidence.json`; no API key, Provider Route, or upstream credential entered the
template, Sandbox, evidence, or logs.

## What remains unproved

- The native lifecycle proof is not yet integrated with Dorf's durable Action settlement or Sandbox
  record. It proves the provider-side recovery primitive, not the second-provider terminal.
- E2B has official JavaScript and Python SDKs but no official Go SDK. The initial Go boundary is a
  handwritten client over the four lifecycle operations in E2B's current
  [control-plane OpenAPI specification](https://github.com/e2b-dev/infra/blob/d586e2e86853b19b156fe7bc1cd76c2ea0b45475/spec/openapi.yml).
  Native command execution over `envd` ConnectRPC remains the next unproved adapter slice.
- The template did not contain the pinned combined Codex/Pi Harness profile or run a Dorf Job.
- The current Provider Gateway binds plain HTTP to a private Incus bridge. No secure remote path from
  E2B to that owner-hosted authority has been designed or tested. This is the principal blocker to
  a real Codex-on-E2B terminal.
- The Hobby one-hour continuous runtime, sustained cost, regional behavior, service reliability,
  and the new custom runtime's detailed isolation guarantees were not evaluated.

Do not call E2B a supported provider until a real adapter preserves Dorf-owned Action settlement,
route creation and revocation, AgentRun recovery, repository evidence, publication, and confirmed
Job cleanup while satisfying D063's import and branch oracle.
