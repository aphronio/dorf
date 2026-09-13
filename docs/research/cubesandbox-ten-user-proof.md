# CubeSandbox: ten users and twenty active workers

Observed on 2026-09-13 with CubeSandbox v0.7.1 and Python SDK 0.7.0. This is non-normative
capacity research, following the [single-runtime proof](retained-runtime-incus-proof.md).
It does not create a supported Dorf profile or establish superiority over Incus.

## Result

The requested active scenario completed: **10 retained assistant environments plus 20 active worker
environments**, all running simultaneously on one disposable CubeSandbox node. All 30 workloads
became ready. Every worker independently advanced its progress counter and consumed CPU in an
additional check. The full-load observation window lasted 60 seconds.

The full pause/resume scenario did not complete. A host-pressure guard stopped the disposable outer
VM during concurrent snapshot operations. This distinction is essential: the running-load measurement
passed; burst creation and concurrent pausing were constrained by this development host.

## Measured footprint

These are medians from the completed sequential-ramp attempt. Samples were collected every five
seconds; the first ten seconds of each phase were excluded from resource summaries.

| State | Total process memory, PSS | Broader system RAM use | Busy CPU cores | Cubelet filesystem used |
| --- | --- | --- | --- | --- |
| Control stack, no sandboxes | 2.11 GiB | 3.27 GiB | 0.07 | 2.25 GiB |
| 10 retained assistants running | 5.77 GiB | 6.88 GiB | 0.41 | 2.43 GiB |
| 10 retained assistants + 20 active workers | **15.40 GiB** | **16.63 GiB** | **2.67** | **2.85 GiB** |

PSS apportions shared process pages rather than counting them repeatedly as summed RSS would.
Broader RAM use is `MemTotal - MemAvailable`, an estimate that includes more than process memory;
it is not the amount permanently unavailable for reclamation. CPU cores are calculated from
`/proc/stat`, excluding idle, I/O wait and stolen time. The measured node had no swap configured.
Filesystem usage includes the warm template/runtime data and small synthetic user files, not an
estimate of real users' repositories or browser profiles. Do not add this nested filesystem's usage
to its outer VM's root-disk usage.

At ten users, the fixed process-memory baseline contributes approximately **0.21 GiB per user**.
The running scenario averages **1.54 GiB process memory per user including two workers and the shared
stack**. These are arithmetic averages for this workload, not recommended per-user quotas. The
20-worker phase added about 9.63 GiB above the ten-assistant phase, or about 493 MiB per worker,
including its deliberately touched 256 MiB heap.

In the full-load phase, the local list API had a 14.1 ms median and 16.7 ms maximum across six probes.
The sampled guest command had an 8.9 ms median and 9.4 ms maximum. These are local control-path
observations, not internet latency or production service-level guarantees.

## Environment and workload

The disposable outer Incus VM used Debian 13, 12 vCPU, 24 GiB RAM and a 110 GiB sparse root disk.
An 80 GiB guest-only XFS reflink loop filesystem supplied Cubelet storage. Native nested KVM was
available. The one-click stack included its databases, object store, DNS, routing and management
services. No production credentials or existing assistant data were inputs.

The official Python code image was pinned to digest
`sha256:467494c38f3c335e42d590e23d5cbb00dc15c2627c9da4f7e7f3bd9f4ec18c5e`.
It provided Python 3.12.12 and SQLite 3.40.1. Every sandbox had a **768 MiB memory limit and 500m CPU
limit**, with one virtual CPU exposed. Guest cgroup checks confirmed those limits. All were created
with expiration disabled.

Cubelet reported default admission quotas of 24,000 millicores and 29,955 MiB memory on this node,
with a maximum of 58 microVMs. Thirty chosen profiles request 15 CPU equivalents and 23,040 MiB,
so they fit without changing admission policies. Thirty whole-CPU profiles would exceed the CPU
quota. Fractional CPU is an enforced limit, not an uncapped reservation. The defaults overcommit
physical resources; fitting admission does not guarantee that every memory/CPU cap can be fully
occupied simultaneously alongside the control stack.

The synthetic workload was explicit:

- Each retained assistant process touched a 128 MiB heap and otherwise remained mostly idle.
- Each worker touched a 256 MiB heap, performed approximately 0.1 CPU-seconds of hashing per second,
  and periodically committed SQLite transactions and rewrote a 1 MiB scratch file.
- Every sandbox held 16 MiB of random persistent user data, home/workspace/tmp markers, and an open
  SQLite database in WAL mode. Progress counters, process identity and a random process nonce were
  recorded for verification.

The independent worker check observed all 20 workers advance counters and CPU time over three
seconds. These were active synthetic processes, not live Codex turns, browser sessions, Docker
builds or compilation workloads. Their memory and CPU profiles must not be treated as measured
production-agent requirements.

Sequential creation included a three-second gap after each workload became ready. The ten-assistant
batch took about 129 seconds and the twenty-worker batch about 390 seconds. Workload readiness
included memory initialization, file transfer, random-file creation and SQLite setup: roughly
9.7–11.3 seconds for assistants and 15.6–16.8 seconds for workers. These are not raw microVM boot times.

## Host-pressure limits and interrupted recovery

An initial attempt created five sandboxes concurrently. The guard stopped only the disposable outer
VM after host full-memory PSI's ten-second average exceeded 2% for three consecutive five-second
samples. Twenty sandboxes had been created and fifteen workloads were ready. The host still had
about 15 GiB available memory; the guest had spare RAM. This was neither a completed thirty-sandbox
trial nor an observed CubeSandbox out-of-memory failure. After restart, those active unsnapshotted
sandboxes were absent from the recovered inventory.

The retry used sequential creation with gaps and completed the thirty-running-sandbox phase.
Starting five concurrent assistant pauses then triggered the same guard. The host still had about
13 GiB available at the trigger. Five pause completions appeared in the live output before the
forced stop. The mixed ten-paused-assistants/twenty-active-workers phase was never reached, so no
steady-state measurement for that combination is claimed.

Both attempts used the same workload, limits and guard thresholds. The physical host's free-page
and cache state also changed after restart; the improved creation run cannot be attributed solely
to sequential ramping. Nested virtualization and an XFS loop filesystem on another copy-on-write
storage layer limit transfer of these observations to bare-metal deployment. The exact cause of
the host pressure was not isolated.

After the second restart, five paused metadata entries survived, matching the five acknowledged
pause completions for users 00–04. **All five resume attempts failed** with CubeMaster error 130483:
`load pause snapshot catalog …: snapshot catalog not found`. No process, marker or SQLite recovery
verification was possible. This establishes failed catalog lookup and unusable paused entries in
this trial; a separate filesystem search was not performed, so the exact missing-file/root-cause
mechanism is not established. The same XFS mount remained configured.

This differs from the earlier single-sandbox clean-reboot result. It prevents treating acknowledged
local pauses as proven crash-durable retention in this setup. The remaining active environments
were also absent after the forced stop. Native S3 snapshots or another off-host backup path were
not configured or tested.

The forced stops interrupted the last log writes. Analysis retained complete records and explicitly
counted invalid trailing data rather than treating the files as cleanly completed runs.

## Interpretation

The control stack's idle footprint alone is not a good reason to reject CubeSandbox: this test
shows it can be shared across ten users with thirty running environments. It remains a serious
candidate when its scheduler and fleet operations reduce work Dorf would otherwise need to supply.

This does not prove that CubeSandbox is more efficient or more scalable than Incus: an equivalent
Incus workload was not measured. Concurrent snapshot behavior, representative Codex/browser memory,
long-duration work, off-host backup recovery and multiple-node scheduling remain separate proofs.
The observed paused-state recovery failure needs resolution before relying on this configuration
for retained assistants.

## Evidence and cleanup

The five unusable paused entries were deleted directly through the native API; all deletes returned
204 and the nested inventory was empty. The task-owned outer Incus VM was then deleted. A separate
inventory query confirmed its absence. No existing production instance was operated on.

Sanitized raw events, workload/analysis scripts, release/image provenance, host-pressure records,
recovery failures and cleanup evidence are retained locally under the ignored
`.dorf/cube-scale-spike/2026-09-13-30-sandboxes/` directory. This document retains the important
findings independently of those local artifacts. Both interrupted attempts remain recorded; neither
is represented as a fully successful end-to-end pause/resume benchmark.
