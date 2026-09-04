# Archived E2B capability proof

Observed on 2026-08-14 with the E2B Hobby account, JavaScript SDK 2.39.0 for capability and template
construction, and Dorf's native Go adapter for lifecycle, execution, and profile qualification. E2B
completed the D063 second-provider Codex coding-to-PR terminal. At that date it was not a supported
deployment profile because its reverse route remained disposable. This file preserves that dated
proof and makes no current support claim. For current support and setup, see
[Support](../support.md) and [Getting started](../getting-started.md).

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

A third live proof used the vendored E2B `process.proto` at infra commit
`d586e2e86853b19b156fe7bc1cd76c2ea0b45475` and generated Connect-Go code. Against hosted envd
0.6.13, the native Go client preserved literal argv, binary stdin, separate raw stdout and stderr,
and exit code 17. A provider-enforced 500 ms process timeout produced E2B's documented
`deadline_exceeded` stream terminal; a separate caller cancellation remained indeterminate with its
observed PID and was explicitly killed once. The proof then deleted the Sandbox and a separate
inventory found no running or paused Dorf E2B proof resources. Connect-Go required a
cancellation-preserving context without the caller's deadline so it would not replace envd's
independent process-timeout header.

A fourth proof extracted the Incus guest provisioning body into one provider-neutral recipe, pinned
Codex 0.147.0 and Pi 0.84.1 to the npm integrities already recorded by the combined Incus artifact,
and built it from the exact Debian 13 amd64 OCI manifest
`sha256:d8f17b92dc7ff10f9c1fdecab0ad21103d1d24aed823c3a0359e4f50adfab3eb`. E2B produced template ID
`gp4l4gthfx5pl5jedixa`, build ID `8bb1103a-c331-4098-9dac-b26a2ed31eae`, and exact runtime reference
`dorf-debian13-combined:8bb1103a-c331-4098-9dac-b26a2ed31eae` from clean source commit
`6e5801b029608656f5b6c2076032befafed4b990`. Dorf's native Go client then created a restricted
Sandbox from that exact reference and attested Debian 13 x86_64, envd 0.6.13, the complete tool
inventory, both Harness executables, root access to `/workspace/job`, absence of npm/npx and baked
Harness or Provider Route configuration, and ownership-guarded cleanup. A final account inventory
contained no running or paused Sandbox.

A fifth proof corrected the native create request to set E2B's independent
`network.allowPublicTraffic: false` ingress restriction in addition to deny-all outbound internet.
The native client then resolved the Codex app-server's in-VM bind URL separately from E2B's external
`wss` port-proxy URL and supplied the provider's scoped traffic token only on the WebSocket upgrade.
Codex simultaneously enforced its own separately generated control capability. After an abrupt
controller disconnect, a second authenticated connection resumed the exact native thread and read
the exact turn ID returned before loss. The turn had no working upstream Provider Route, so this
proves endpoint transport and native controller recovery, not a successful model outcome. All
iterations were ownership-deleted and the final running/paused account inventory was empty.

A sixth proof reran that authenticated-endpoint scenario through the extracted
`internal/sandbox.Sandbox` contract rather than calling E2B lifecycle, envd, or endpoint types from
Codex. The common ownership tuple drove create reconciliation, every command, endpoint resolution,
reconnect, and deletion; the provider-generated ID and traffic capability remained inside the E2B
adapter. Codex recovered native thread `019fffed-15cc-7372-8f47-2e63f7017cfa` and turn
`019fffed-1702-7412-88dd-300a441f0ebc` after abrupt controller loss, then the adapter deleted the
Sandbox and observed its exact ownership query absent. This still used only a proof marker rather
than a working Provider Route, so it does not claim a model outcome or supported E2B profile.

A seventh proof kept the Provider Gateway broker on its private Incus bridge and placed a disposable
outbound HTTPS tunnel in front of it. E2B admitted only that exact hostname over otherwise denied
egress. The common terminal layer installed one consumer-scoped route; an anonymous request from the
Sandbox received 401, while unchanged Codex completed a real `gpt-5.6-sol` turn through the scoped
route. Cleanup revoked the route before deleting the Sandbox, and independent inventory found zero
proof routes and zero exact E2B resources before the tunnel stopped.

An eighth proof selected E2B at Dorf's composition root and admitted one durable no-change Codex Job
against the exact remote repository Revision. It settled Sandbox create, clone, setup, and route
Actions, then exposed a missing `sandbox_id` in the PostgreSQL delivery projection before model
submission. After that field was restored, the same durable Job adopted its existing provider
resources, bound and completed one native thread and turn, and recorded exact unchanged Git-revision
Evidence. Explicit cleanup settled route revoke before Sandbox delete and completed its Absurd task.
An independent final check found zero matching running or paused E2B Sandboxes and zero Gateway
routes. The disposable tunnel stopped. General Sandbox internet access was explicitly enabled for
this Job because clean repository setup follows changing dependency and redirect hosts; E2B remains
default-deny when that profile option is absent.

A ninth proof admitted a documentation-only modifying Job from exact remote Revision
`b95373166af6a0ffee743c3c5840dedaa95a8c6f`. Codex committed Revision
`3183e618329abd72f0697fd3a9f83e86a94dc91d`; both repository Checks passed, policy correctly selected
no agent review, and Dorf pushed the exact branch and opened PR #134. GitHub CI passed. Closing that
PR produced a rejected Outcome, after which Dorf revoked the scoped route before deleting the main
Sandbox and completed both its run and cleanup tasks. Independent inventory found no matching E2B
Sandbox and no Gateway route.

A tenth proof admitted a focused E2B endpoint security-test Job from the same base. Codex added
`internal/e2b/endpoint_test.go`, committed exact Revision
`764f9312dd217b149e393c0ae1f31696894f4174`, and passed both repository Checks. Review policy selected
one general Role because the non-documentation change was not mechanically classified. Dorf created
a separately owned E2B reviewer Sandbox, transferred the immutable checkout by Git bundle, installed
an independent scoped Provider Route, and ran Codex with immutable read-only capability. The reviewer
returned no-material-issue feedback with observed Revision-bound Evidence; an unchanged follow-up
settled on the original implementation thread. Dorf published PR #135, GitHub CI passed, and the PR
merged as `f6961465f89b383d3ae0e84a207f64c4f2fba925`. Dorf observed the accepted Outcome, revoked the
main route before deleting the main Sandbox, revoked the reviewer route before deleting the reviewer
Sandbox, and completed both Absurd tasks. Independent final queries found zero Job-owned running or
paused E2B Sandboxes and zero Gateway routes before the disposable tunnel stopped. No tunnel
hostname or credential is recorded here.

The broad capability template is named `dorf-capability-20260814-v1`; the combined Harness profile
uses the exact build reference above. Detailed machine-readable capability evidence and the exact
profile manifest are retained locally under ignored `.dorf/e2b-spike/evidence.json` and
`dist/e2b-template/profile.json`. No API key, Provider Route, or upstream credential entered either
template, Sandbox, evidence file, manifest, or logs.

## Unproved as of 2026-08-14

- E2B has official JavaScript and Python SDKs but no official Go SDK. The initial Go boundary is a
  handwritten client over the four lifecycle operations in E2B's current
  [control-plane OpenAPI specification](https://github.com/e2b-dev/infra/blob/d586e2e86853b19b156fe7bc1cd76c2ea0b45475/spec/openapi.yml).
  The command client is generated only from the pinned upstream process schema; handwritten code
  retains Dorf's timeout, cancellation, retry, output, and error semantics.
- Pi on the exact combined template has not run a Dorf Job; the E2B runtime profile currently admits
  Codex only.
- No provider file-transfer or durable artifact API has been implemented. A concrete workflow must
  earn that surface rather than extending the command client speculatively.
- The tested reverse path uses an unsupported disposable Quick Tunnel. A stable deployment-owned
  HTTPS route and domain remain unselected.
- The Hobby one-hour continuous runtime, sustained cost, regional behavior, service reliability,
  and the new custom runtime's detailed isolation guarantees were not evaluated.

As of 2026-08-14, E2B had completed the provider-neutral coding-to-PR proof, but the disposable-tunnel
profile was not supported. Stable reverse-route operations and their recovery behavior had not been
proved. [Support](../support.md) and [Getting started](../getting-started.md) own the current status.
