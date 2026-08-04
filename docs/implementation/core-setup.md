# Core Setup and Summon DX

- **Status:** Accepted product direction; configured-host terminal plus Arch and Ubuntu 24.04
  host-convergence recipes complete, public image acquisition pending
- **Scope:** Dorf core only—Worker, Room, Job, Assignment, the built-in Incus and Codex
  adapters, and the model connection required to run a real Worker
- **Terminal:** A stranger on one supported Linux host can install Dorf, run one guided setup,
  summon a real Worker with no flags, complete a real turn, and destroy every disposable resource

This guide records the intended first-run and summon experience for the Dorf core. It is the
durable implementation handoff for making the North Star's summon step boring on a fresh machine.
The mutable checklist and implementation discussion belong in the linked GitHub issue.

The coding-to-PR showcase is deliberately outside this plan. Repository contracts, GitHub Apps,
cloning, branches, review agents, checks, publication, and AFK orchestration must not enter the
core setup path. Once the core Worker/Room/Job loop is excellent, the coding workflow can compose
on it as a separate product layer.

## Outcome

The public happy path is:

```bash
uv tool install dorf
dorf setup
dorf worker spawn ada
```

`dorf setup` is one guided, resumable command. It installs and configures the supported local
dependencies after explaining consequential machine changes, installs the official Dorf Room
image, connects a model provider, and proves the complete path through a disposable real Worker
turn. After setup, ordinary Worker commands do not require a repository, `.dorf.toml`, image
alias, adapter choice, or provider flag.

The default summon should feel like:

```text
$ dorf worker spawn ada

ada · ready
Room prepared in 7.2s
Provider: personal-chatgpt

Next:
  dorf job assign JOB --to ada --goal "..."
```

Provider names, Incus instance names, image fingerprints, route IDs, native conversation IDs, and
protocol diagnostics remain available through explicit detailed inspection and logs. They are not
the normal front door.

## Core boundary

The setup path may compose:

- `dorf.runtime`;
- the in-process Dorf facade;
- the built-in Incus Environment adapter;
- the built-in Codex Agent adapter;
- the Provider Gateway connection and Room route needed for a real Codex turn; and
- Dorf-owned host configuration, image, diagnostics, and setup presentation.

It must not require or configure:

- a managed Git repository or `.dorf.toml`;
- the coding workflow;
- GitHub authentication;
- branch, PR, check, review, or publication policy;
- Droid or another review agent;
- AFK orchestration;
- a second sandbox or harness;
- a plugin/provider matrix; or
- a durable workflow engine for installation.

Generic Worker and Job operations use the global Dorf deployment profile. Only coding-workflow
commands may consult a repository contract. Running `worker spawn` from inside an arbitrary Git
repository must not change which image, environment, model connection, or runtime defaults it uses.

## One setup command

The normal setup command has no required flags and asks only questions that materially affect the
machine or trust boundary:

```text
$ dorf setup

◆ Dorf
  Durable workers in private local Rooms

Checking this machine
✓ Linux · x86_64
✓ Hardware virtualization available
! Incus is not installed

Incus provides the isolated virtual machines Dorf calls Rooms.
Installing it adds a local virtualization service and gives your user
permission to manage its VMs. It does not expose the service remotely.

Install Incus now?
● Yes, install it
○ No, show me the instructions
```

Before invoking an administrator boundary, setup shows the concrete effect:

```text
Dorf needs administrator permission to:

• install the Incus packages
• start the local Incus service
• add your user to the incus-admin group

No remote API will be enabled.

Continue? Yes
```

The remaining happy path is:

```text
✓ Incus installed
✓ Local storage initialized
✓ Private VM network ready

Room image
● Dorf Codex — recommended
  Latest Codex validated by Dorf

↓ Downloading Dorf Codex
✓ Image signature verified
✓ Image ready

Model connection
● ChatGPT subscription
○ OpenAI API key

Open https://... and enter ABCD-EFGH
✓ Connected
✓ Saved as the default

Verifying the complete Worker loop
✓ Disposable Room created
✓ Codex completed a real turn
✓ Provider route revoked
✓ Disposable Room destroyed

Dorf is ready.

Next:
  dorf worker spawn ada
```

Setup is not a TUI product of its own. It is a calm interactive CLI with good defaults, bounded
progress, concise explanations, and a copyable next action.

## Official Dorf Room image

Dorf publishes and maintains a credential-free, Codex-ready Incus VM image. Users do not build
an image on the default path.

The official image channel means:

> Latest Codex successfully validated by Dorf.

The image publication pipeline:

1. rebuilds whenever a new Codex version is selected for validation;
2. installs the latest Codex release rather than imposing a permanent user-facing Codex pin;
3. records the exact installed version and package digest as evidence;
4. verifies that no upstream provider credential, route key, generated machine identity, or
   ambient host secret is present;
5. runs the exact Codex app-server operations Dorf depends on;
6. runs a real Dorf Worker turn through the supported Provider Gateway path;
7. publishes only after the compatibility and credential-boundary checks pass; and
8. signs or otherwise strongly authenticates the image manifest and immutable image digest.

New Rooms use the currently promoted official image. Existing Rooms retain the image and Codex
version they were created with until explicitly ended; Dorf never mutates a live Worker's
harness underneath its native conversation.

If a new Codex release fails compatibility validation, the publication pipeline fails visibly and
does not replace the promoted image. This is an upstream compatibility finding, not a reason to
silently alter running Rooms. Exact version recording exists for diagnosis and reproducibility of
findings, not as a policy to hold users indefinitely on an old Codex release.

### Official image only

The initial setup supports only the Dorf-published Room image. It does not offer a custom-image
selector or accept responsibility for validating and supporting arbitrary Incus images.

This keeps one credential boundary, Codex compatibility contract, update path, and support surface
for the first public release. Existing Rooms remain bound to their recorded immutable image. Custom
images can be reconsidered only after a concrete user need justifies the additional validation and
maintenance burden.

## Host installation and support posture

Dorf should set up the host for the user on explicitly supported Linux distributions. Setup
detects the OS from stable host facts, presents the reviewed package/service operations, asks at the
administrator boundary, applies them, and verifies observed reality.

Initial host support should be narrow and honest. Each supported distribution has one concrete,
tested recipe covering:

- Incus package installation;
- service enablement and startup;
- `incus-admin` group creation/membership;
- any required session restart or group refresh;
- KVM device availability and access;
- minimal local storage initialization;
- a private local VM network without enabling the remote Incus API; and
- bounded disk, memory, architecture, and virtualization checks.

The first implementation should support the owner's real host plus one common clean Linux
installation terminal. Other Linux distributions receive the official upstream installation link,
a precise diagnosis, and the agent-assisted recovery path below until their recipe is validated.
An unsupported OS is reported as a support limitation, not disguised as a generic setup failure.

Dorf owns its setup recipes and verification. It does not fork Incus packaging, install a
different virtualization stack, or claim that a client-only Incus installation on a non-Linux host
provides a local Room daemon.

## Idempotence without a workflow engine

Setup is a bounded local convergence operation. It does not require Absurd, Postgres, Temporal,
Ansible, or a second setup-state database.

The machine and existing Dorf authorities already contain the durable checkpoints:

| Question | Durable observation |
| --- | --- |
| Is Incus installed? | executable, package, and service facts |
| Can this user operate it? | socket access and group membership |
| Is Incus initialized? | server, storage, profile, and network facts |
| Is the Room image ready? | immutable fingerprint and validated metadata |
| Is the provider connected? | Provider Gateway connection authority |
| Does the complete path work now? | a new disposable real Worker smoke |

Each setup operation follows:

```text
inspect → already correct?
            yes → render success
            no  → explain and request permission if consequential
                  apply the smallest concrete change
                  verify observed reality
```

The initial implementation should remain a direct sequence of concrete operations, for example:

```python
ensure_supported_host()
ensure_incus_installed()
ensure_incus_initialized()
ensure_dorf_room_image()
ensure_default_provider_connection()
verify_disposable_worker()
```

Do not create a generalized setup-provider protocol until a second Environment has a materially
different setup path. Do not treat a stored “step completed” bit as proof when the corresponding
machine resource can be inspected.

Ctrl-C, process failure, package-manager failure, network loss, and a required new login are normal
pause points. The final message says that setup is paused and that rerunning `dorf setup` will
reinspect and continue. Temporary builder, probe, and smoke resources use exact identities and are
reconciled on retry.

## First-class CLI quality

The CLI is a primary product surface. Its setup and lifecycle commands follow these rules:

- pressing Enter follows the recommended happy path;
- choices appear only when the answer changes a meaningful product or trust decision;
- consequential changes receive a short explanation before approval;
- progress reflects real bounded stages rather than fake activity;
- raw subprocess and protocol output is captured to diagnostics rather than streamed by default;
- colors and symbols improve scanning but meaning never depends on color;
- errors name the observed cause and one concrete next action;
- Ctrl-C is safe and explains how to resume;
- success ends with the most useful next command;
- human flows use Worker and Job names, not infrastructure handles; and
- detailed infrastructure remains available for break-glass diagnosis.

The desired taste is the shortest path of polished developer CLIs such as Vercel and the
single-wizard/real-verification posture of Hermes setup, adapted to Dorf's stronger host,
isolation, cleanup, and provenance requirements.

## First-class agent support

Machine-level problems are inevitable across kernels, distributions, package repositories,
virtualization settings, permissions, networks, and hardware. The CLI is the primary support
protocol for humans and agents alike. Codex, Claude Code, or another coding agent should be able to
run the same `dorf setup` and `dorf doctor` commands, understand the bounded result, apply
safe remediation, ask before consequential changes, and verify the outcome without first reading a
large troubleshooting manual.

Agent friendliness belongs in command behavior and structured diagnostic output. Documentation is a
small fallback for durable security boundaries, unsupported cases, and upstream ownership that
cannot be explained adequately in one command result.

### Diagnostic contract

Every setup failure has:

- an owning boundary (`host`, `packaging`, `incus`, `dorf`, `codex`, `provider-gateway`, or
  `unknown`);
- a classification (`configuration`, `unsupported`, `transient`, `compatibility`, or
  `possible-upstream-regression`);
- bounded observed facts;
- expected versus actual behavior;
- safe remediation;
- a minimal reproducer when available; and
- the appropriate reporting destination.

For example:

```text
Setup paused

Dorf found /dev/kvm, but your user cannot open it.

Human-readable diagnostic:
  ~/.local/state/dorf/diagnostics/.../diagnostic.md
Agent-readable diagnostic:
  ~/.local/state/dorf/diagnostics/.../diagnostic.json
```

Setup and `dorf doctor` render the useful diagnosis directly and produce a bounded bundle for
longer evidence:

```text
diagnostic.md       human-readable situation
diagnostic.json     agent-readable structured observations
commands.log        bounded, redacted command outcomes
```

The bundle may include:

- OS ID and version, kernel, and architecture;
- bounded CPU virtualization availability;
- `/dev/kvm` existence and permissions;
- Incus client/server versions and service status;
- relevant user group membership;
- Dorf-owned storage, network, profile, image, and Room facts;
- official image fingerprint and Codex version;
- sanitized Provider Gateway health;
- failed argv identity, exit code, and redacted error; and
- exact cleanup state for temporary resources.

It must exclude:

- environment-variable values;
- upstream provider credentials and OAuth documents;
- inference route and management keys;
- Git credentials and private keys;
- arbitrary home-directory content;
- complete process environments; and
- unbounded host or third-party logs.

Redaction is behavior protected by tests, not a documentation promise alone.

### Agent-friendly command behavior

The normal diagnostic loop is:

```text
dorf setup
→ bounded failure with ownership and observed facts
→ dorf doctor
→ observed facts and the smallest safe next action
→ approval request only for consequential machine changes
→ dorf setup
→ real Worker verification
```

Both interactive and non-interactive callers receive the same semantic fields:

```text
status
owner
classification
summary
observed
expected
safe_actions
approval_required_actions
reproducer
diagnostic_path
```

Human rendering may be polished and conversational, while structured rendering remains available
for callers that need it. The fields originate in the implementation, not in a second
hand-maintained error catalog. CLI help is the command syntax authority.

Failure output labels the Markdown as human-readable and the JSON as agent-readable. It does not
emit agent-directed instructions or a copyable prompt; agents act under their existing user and
system instructions. An optional packaged troubleshooting skill may follow only if repeated
dogfood proves the CLI result is
insufficient; it must never be required to diagnose ordinary setup.

### Minimal durable documentation

Documentation should cover only information that is stable, safety-critical, and too broad for one
command result:

- the supported host matrix and what “unsupported” means;
- the administrator and `incus-admin` trust boundary;
- the diagnostic/redaction contract;
- the ownership boundary between host packaging, Incus, Dorf, Codex, and the Provider Gateway;
  and
- how to prepare and review an issue report before anything leaves the machine.

Prefer one compact agent-support page linked from CLI output over a tree of per-error pages. Do not
copy package-manager commands, current versions, error messages, or remediation steps into prose
when setup and doctor can derive and render them from current observed state. The docs site may
publish a small `llms.txt` index, but the installed CLI remains sufficient offline and is the source
of truth.

### Upstream triage

The agent documentation distinguishes:

- a minimal ordinary Incus operation failing outside Dorf: Incus or distribution packaging;
- Codex app-server behavior failing in the current official clean image: Codex compatibility or
  upstream;
- broker/provider authentication failing independently of a Room: Provider Gateway/backend or
  upstream provider path;
- Dorf recording incorrect state, leaking a secret, failing to converge, or leaving resources:
  Dorf;
- virtualization disabled in firmware or inaccessible hardware: host configuration; and
- an unvalidated OS or architecture: unsupported host.

Dorf and its agent documentation may prepare an issue-ready report, but never submit host
details to Dorf or an upstream tracker without the human's explicit approval.

## Global deployment profile

Successful setup records only the choices and immutable identities required for later core
operations, under the ordinary XDG configuration/state boundaries. At minimum:

- selected Environment type;
- official image identity and immutable fingerprint;
- selected default Provider Connection name;
- Dorf-owned Incus resource names needed to inspect or clean exact state; and
- setup/image compatibility metadata required for honest diagnostics.

The profile does not duplicate provider credentials, Room lifecycle, Worker/Job state, or image
contents. Those remain with their existing authorities. Removing or corrupting the profile must not
cause Dorf to guess which external resources it owns.

## Implementation slices

Implementation should proceed through runnable vertical slices.

### 1. Core configuration boundary

- [x] Stop consulting `.dorf.toml` for generic Worker and Job operations.
- [x] Add the smallest global deployment profile needed by the built-in composition.
- [x] Let the current provider-connect path choose a default Provider Connection so
  `worker spawn NAME` needs no provider flag; the guided setup will own this choice when it lands.
- [x] Retain the explicit Provider Connection override for current dogfood and repair.

Terminal: on the current configured machine, `worker spawn NAME` works from outside a Git repository
with no options and reaches a real Worker turn.

### 2. Official image publication and consumption

- [x] Separate the public credential-free Codex Room image from private workflow/reviewer images.
- [x] Build the latest Codex image in an automated candidate pipeline.
- [x] Validate the credential boundary and real Worker terminal.
- [x] Produce the authenticated image digest and small compatibility manifest.
- [x] Implement consumption that requires an immutable release and verifies GitHub asset digests,
  manifest metadata, the downloaded archive, and the post-import Incus fingerprint.

Terminal: a machine without a local Dorf image obtains the official image and completes a real
disposable Worker turn without building the image locally.

The implementation-side candidate terminal passed on 2026-07-31 with Codex 0.146.0: a fresh
credential-free VM completed the expected real Worker response through the Provider Gateway, the
route and Room were removed, and the exported archive digest matched its generated Incus fingerprint
and manifest. This is not the slice terminal: anonymous consumption cannot be demonstrated while
the repository is private, and setup does not call the consumer yet.

#### Public activation checklist

The anonymous distribution terminals genuinely require a public repository. GitHub allows release
immutability and runner registration while a repository is private, but those operational changes
are deliberately postponed and batched with the visibility change rather than treated as technical
blockers.

Activation-window preparation:

- [ ] Enable GitHub immutable releases and set `DORF_IMMUTABLE_RELEASES_ENABLED=true`.
- [ ] Register the dedicated x86_64 Incus image runner with the `dorf-image` label and configure
  its validated `DORF_IMAGE_PROVIDER_CONNECTION`.
- [ ] Invoke the already verified official-image consumer from guided setup after the first release
  exists.

Public-only acceptance:

- [ ] Promote the first publicly accessible complete `room-image-*` release from the dedicated
  x86_64 Incus runner.
- [ ] From an unauthenticated client, confirm the Releases API reports `immutable: true`, exactly one
  archive and manifest for x86_64, and GitHub SHA-256 digests for both.
- [ ] Download both assets without GitHub credentials and verify the release attestation, asset
  digests, manifest, archive digest, and Incus fingerprint.
- [ ] Activate the official-image consumer in guided setup and run it on a clean supported host with
  no local Dorf image and no GitHub authentication.
- [ ] Complete a real disposable Worker turn on that anonymously obtained image and verify exact
  Room and Provider Gateway route cleanup.
- [ ] Rerun setup against the same promoted fingerprint and prove it performs no image download or
  mutation.
- [ ] Promote a later validated Codex image and prove new Rooms select it while an existing Room
  retains its original image and native conversation.
- [ ] Only after those terminals pass, advertise the official image and no-local-build setup path in
  public installation documentation.

Repository visibility is a consequential owner action and is not performed by the implementation
workflow. The activation-window preparation is not visibility-blocked, but is postponed by product
sequencing. The anonymous distribution and stranger terminals cannot run while the repository is
private.

### 3. Guided host setup

- [x] Add the no-option `dorf setup` command and stage-oriented rendering.
- [x] Inspect Linux architecture, Incus service access, the private VM network, the exact local
  image fingerprint, and the selected Provider Connection from their real authorities.
- [x] Stop before host mutation or VM launch unless x86 virtualization, `/dev/kvm`, at least 4 GiB
  total memory, and at least 20 GiB free on the supported default root filesystem are present.
- [x] Save the exact validated image fingerprint in the global deployment profile without
  duplicating credentials.
- [x] End setup with a real disposable Worker turn, route revocation, and Room destruction.
- [x] Reinspect and reverify on rerun without rewriting unchanged configuration or trusting a
  completion flag.
- [x] Bring Provider Connection choice and login into the guided setup command: pressing Enter
  selects the recommended ChatGPT subscription device flow, while OpenAI API key remains the
  explicit alternative; setup supplies the stable connection name.
- [x] Implement the first concrete Incus installation recipe for Arch Linux using the distribution
  package, local service, and `incus-admin` group.
- [x] Implement Ubuntu 24.04 LTS convergence using its native `incus` and `qemu-system` packages,
  the same reviewed systemd service and `incus-admin` boundary, and no third-party install script.
- [x] Explain the full Arch package update, local service, and root-equivalent group boundary, then
  request approval before administrator authentication.
- [x] Initialize a pristine daemon with Incus's minimal local storage and private `incusbr0`
  network, verify that no remote API was enabled, and refuse to overwrite partially initialized
  installations.
- [x] Reconcile interruption across the reviewed Arch package, service, and `incus-admin` changes
  by inspecting each real checkpoint, requesting approval only for missing privileged changes, and
  refusing to guess ownership of ambiguous Incus resources.
- [x] Detect a stale local Incus daemon left running after a package update, explain the mismatch,
  and request approval to restart only the local service before attempting Room work.

Terminal: a clean supported Linux host reaches the same disposable Worker terminal through
`dorf setup`.

The configured-host terminal passed repeatedly on 2026-07-31 with no setup options. It reused
`dorf-codex` fingerprint `696b1612db90...` and Provider Connection `personal-chatgpt`, completed
the exact real Codex response, revoked the route, destroyed the Room, left no setup VM, and did not
rewrite the unchanged deployment profile. Until the remaining items land, a missing Incus service,
network, or image pauses with one bounded next action rather than attempting an unreviewed machine
change. When no Provider Connection exists, the same setup command now guides the choice and login
before running the terminal.

The real preflight observed KVM plus CPU virtualization, 61 GiB total memory, and 176 GiB free
before touching Incus. The minimums are deliberately conservative for one local Room rather than a
capacity planner: 4 GiB total memory and 20 GiB free on `/`. Failure stops before package,
service, network, image, provider, or Room mutation and produces the normal bounded diagnostics.

The Arch recipe was exercised on 2026-07-31 inside a clean current Arch VM with nested KVM. It
installed the official `incus` package through a full Arch system upgrade, enabled the local
service, and ran `incus admin init --minimal`. Verification observed a created directory-backed
default storage pool, managed private `incusbr0` bridge, default profile wiring, and an empty
`core.https_address`. The disposable validation VM was removed. On a fresh non-root login, setup
pauses after installation only when `incus info` proves that the new `incus-admin` membership is
not effective yet; signing out and back in, then rerunning the same command resumes from observed
state. The clean-host terminal remains intentionally incomplete until setup can anonymously obtain
the official image after the repository is public.

Interrupted Arch host setup now resumes from observed state rather than from a completion marker.
If the package exists but `incus info` fails, setup separately inspects whether `incus.service` is
enabled and active, whether the user's `incus-admin` membership is configured, and whether that
membership is effective in the current login. It offers only the missing service/group changes,
performs no mutation when declined, and rechecks Incus access afterward. A configured but
not-yet-effective membership produces the unavoidable sign-out/sign-in pause without another
administrator operation. Partially initialized Incus storage or networking remains an explicit
stop: setup cannot establish that those generic resources belong to Dorf and will not
commandeer them.

The Ubuntu 24.04 recipe was exercised on 2026-08-04 in a clean nested-KVM VM. Ubuntu's native
Incus 6.0 and QEMU packages installed, `incus admin init --minimal` created the directory-backed
pool and private `incusbr0`, and the remote API remained disabled. The pristine root-authority run
reached the intentionally inactive public-image boundary in 44.213 seconds. A fresh ordinary user
with the expected `incus-admin` membership then reached the same bounded boundary in 0.235 seconds.
The non-root approval, declined mutation, new-login pause, and observed-state resume transitions
remain protected by behavior tests; the complete clean-host Worker terminal still awaits the first
public image.

The same nested host compared cached Ubuntu VM guest readiness on Incus's default `dir` pool
(15.888, 15.488, and 15.490 seconds; 15.490-second median) with a disposable loop-backed Btrfs pool
(12.425, 12.429, and 11.434 seconds; 12.425-second median). The roughly three-second improvement
does not justify another package, loop-backed filesystem, or automatic storage decision on a
stranger's machine. Initial setup therefore retains Incus's robust minimal `dir` default. Repeat
the measurement with the promoted Dorf image on non-nested supported hosts; reconsider if warm Room
readiness repeatedly exceeds ten seconds and storage is shown to dominate the delay.

### 4. Agent diagnostic contract

- [x] Define compact structured setup diagnosis and ownership for the guided setup path.
- [x] Make setup and doctor use the global core deployment boundary and emit the same bounded
  human-readable Markdown and agent-readable JSON diagnostics.
- [x] Generate self-contained Markdown/JSON/log bundles with tested terminal-and-file redaction.
- [x] Publish only the minimal durable support, security-boundary, and upstream-ownership documentation
  that cannot live in the CLI.

Terminal: an induced setup failure emits the correct owner, classification, bounded evidence, and
safe action in both human-readable and agent-readable files; credential-shaped values are redacted,
private permissions are enforced, and rerunning setup can reach the real Worker verification
without leaking a Room or provider route. Running particular third-party coding agents against the
files is not a release gate.

Setup stops now render a bounded summary and next action. They create a private directory under
`$XDG_STATE_HOME/dorf/diagnostics/` (or `~/.local/state/dorf/diagnostics/`) containing
`diagnostic.md`, `diagnostic.json`, and `commands.log`. The terminal labels the first file as
human-readable and the second as agent-readable. The JSON contains the shared semantic fields
described above. Credential-shaped values are
redacted before terminal rendering and again before persistence; tests protect both boundaries and
0600 file/0700 directory permissions. The first implementation intentionally records that no raw
command transcript was captured rather than copying arbitrary subprocess output.

The core doctor terminal passed on the configured host on 2026-07-31. It read the global deployment
profile, reported sanitized Provider Gateway health, launched the configured Room image on its
private network, verified the Incus guest agent plus DHCP, DNS, and outbound TCP, and removed the
exact disposable probe. It does not read `.dorf.toml`, install Docker, or apply coding-workflow
requirements. Failures use the same simple diagnostic files as setup.

### 5. Stranger acceptance

On a clean supported machine, record:

```text
install Dorf
→ run guided setup
→ install/configure Incus with informed approval
→ obtain official image
→ connect provider
→ prove disposable Worker
→ spawn named Worker without flags
→ assign and inspect a non-coding Job
→ end Job
→ end Worker
→ verify exact Room and route cleanup
```

The terminal is incomplete if it depends on the owner's pre-existing image, credentials,
`.dorf.toml`, undocumented host state, direct Incus repair, raw protocol logs, or a coding
workflow operation.

## Acceptance bar

The first public core setup is ready when:

- the normal path is one package-install command followed by `dorf setup`;
- the setup command has no required flags;
- every administrator action is explained and explicitly approved;
- no local image build is required on the default path;
- the recommended image contains the latest Codex validated by Dorf;
- provider connection is one-time and a default is saved;
- setup ends in a real disposable Worker turn and exact cleanup;
- `dorf worker spawn NAME` works without repository context or required options;
- setup can be interrupted at every stage and safely resumed by rerunning it;
- common failure output is calm, bounded, actionable, and secret-free;
- every failure emits a portable agent-readable diagnostic;
- setup and doctor are sufficient for ordinary agent-assisted repair without external documentation;
- an agent can distinguish host configuration, unsupported environment, Dorf defect, and likely
  upstream failure;
- the supported host matrix is explicit and evidence-backed; and
- no coding-to-PR semantics or configuration entered the core setup path.

## Non-goals

- Supporting every Linux distribution in the first release
- Local Incus daemon support on macOS or Windows without a validated Environment
- Automatically editing firmware or BIOS virtualization settings
- A universal package manager or installation DSL
- A durable setup workflow service or Postgres dependency
- Automatically submitting diagnostic data or upstream issues
- Custom Room images in the initial public setup
- Installing coding workflow, GitHub, review, or AFK configuration
- A second Environment, harness, provider registry, or plugin framework
- Hiding exact setup or cleanup failures behind a generic success message

## Reconsider when

- a second Environment demonstrates a concrete setup seam that should be extracted;
- repeated distribution recipes prove a small shared package/service abstraction;
- official image distribution cannot meet size, authenticity, or update requirements;
- Codex publishes a supported distribution/update contract that simplifies the rolling image;
- a concrete custom-image need justifies a second compatibility and support surface;
- real agent-assisted repairs require a packaged skill beyond the portable diagnostic documents; or
- a remote or multi-user Dorf authority makes local host convergence the wrong setup boundary.
