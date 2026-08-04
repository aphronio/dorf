# Getting started with Dorf

This guide exercises the supported Dorf core: one Codex Worker in one local Incus VM Room, assigned
one non-coding Job. It does not configure a repository or use the coding-to-PR workflow.

> [!NOTE]
> This walkthrough was validated with Dorf `0.1.1`. Dorf is alpha software, and command syntax may
> change in a later release.

## Prerequisites

Use a local x86_64 Linux host with:

- hardware virtualization enabled and an accessible `/dev/kvm`;
- at least 4 GiB total memory and 20 GiB free on `/`;
- `uv` and Python 3.11 or newer available to `uv`;
- network access for PyPI, the immutable GitHub Room-image download, provider login, and inference;
- either a ChatGPT subscription, which is the live-validated and recommended setup choice, or an
  OpenAI API key, which is also an implemented setup option; and
- administrator access if Dorf needs to install or configure Incus.

Automatic host convergence has been reviewed for x86_64 Arch Linux and Ubuntu 24.04 LTS. On Arch,
installing Incus includes the package update required by the rolling-release package set. On Ubuntu,
it uses the distribution's native Incus and QEMU packages. Setup explains the exact changes and
asks before using administrator authority.

Other x86_64 Linux distributions may work only when a local Incus daemon is already installed,
initialized, and usable by the current user. Read [Setup support](support.md) before proceeding on
another host. Membership in `incus-admin` is root-equivalent machine access.

## Install the verified release

```bash
uv tool install "dorf==0.1.1"
dorf --version
```

This guide is version-specific; `dorf --version` should print `0.1.1`.

## Set up the host and provider

Run the no-option guided setup:

```bash
dorf setup
```

The first run downloads an approximately 766 MB Room image. It may also request administrator
authority to install or configure Incus and require a sign-out/sign-in before the new
`incus-admin` membership takes effect.

Setup inspects the host before mutation. When needed on a reviewed distribution, it offers to
install and start Incus, add the user to `incus-admin`, and initialize local storage plus the
private `incusbr0` NAT network. It does not enable Incus's remote API. It then installs or reuses the
immutable credential-free Dorf Room image, selects or connects the chosen provider, and runs a real
turn in a disposable Worker. The disposable Room and its scoped provider route are removed
afterward.

For the recommended ChatGPT subscription path, follow the displayed device URL and code. The other
choice reads `OPENAI_API_KEY` when present or prompts for the key without echoing it.

Setup is complete only when it prints `Dorf is ready.` and offers `dorf worker spawn my-worker` as
the next command. Earlier green checks are not the success boundary. Rerunning `dorf setup` is safe:
it rechecks the host, image, and provider, reuses the verified image when present, and repeats the
disposable real-Worker proof.

If setup asks for a new login so `incus-admin` membership can take effect, sign out and back in,
then rerun the same command.

## Run one Worker and Job

Spawn a durable Worker and its private Room:

```bash
dorf worker spawn guide-worker
```

Success includes `guide-worker · ready` and `current Job: none`. Spawning does not send a model
turn or create a placeholder Job.

Assign a complete goal. Assignment queues the goal as input 1 and returns while delivery continues
detached:

```bash
dorf job assign first-job \
  --to guide-worker \
  --goal "Create a five-item checklist for reviewing a technical proposal. Return only the checklist."
```

Success includes `first-job · open`, the exact `goal v1`, and the workspace
`/workspace/jobs/first-job`. Wait for the latest input selected when the command begins to leave the
working state:

```bash
dorf job wait first-job
```

The completed boundary is `Job first-job: done` followed by the model's `Response:`. A `blocked` or
`pending-approval` outcome is not success; read its `Need:` or `Detail:`. `wait` is read-only and
does not ask the Worker for a status update.

Inspect the recorded situation independently:

```bash
dorf job inspect first-job
dorf worker inspect guide-worker
```

At this point the core loop is proven: a named Worker and Room survived the initiating commands, a
goal-backed Job ran in its dedicated conversation, and Dorf returned the native response.

## Leave, reconnect, and steer

Closing the initiating terminal does not delete the Worker, Job, admitted input, native
conversation binding, or Room. On the same host, reconnect through the recorded names:

```bash
dorf job inspect first-job
dorf job inspect first-job --timeline
dorf job inspect first-job --evidence
```

The default view reports observed runtime facts separately from accepted Worker claims. Timeline and
evidence are retained Job-document lenses; neither starts a turn.

Steer the same Job with an ordinary message, then wait for the newly admitted latest input:

```bash
dorf job message first-job "Revise item three to include explicit risk and rollback checks."
dorf job wait first-job
```

The message is queued durably and does not change goal version 1. For automation, add `--json` to
`job message`, retain its `message_id`, and pass that value to `job wait --message ID` so the wait is
pinned to one admitted input.

To enter the current Room directly, use:

```bash
dorf worker attach guide-worker
```

The shell starts at `/workspace`; the Job workspace is `/workspace/jobs/first-job`. Exiting the
shell ends human presence without changing Worker, Room, Job, Assignment, or conversation identity.

After a controller interruption or host restart, reconcile the exact surviving Room and restart
replaceable delivery processes with:

```bash
dorf worker recover guide-worker
```

Recovery never invents a replacement Room. Inspect and wait again after it completes.

## Clean up

Wait for the latest Job input to settle, then end the Job before the Worker:

```bash
dorf job wait first-job
dorf job end first-job
dorf worker end guide-worker
```

Successful Job cleanup reports `Ended Job: first-job` and removes
`/workspace/jobs/first-job` from the Room. Successful Worker cleanup reports
`Ended Worker: guide-worker` and `Room destroyed:`; destruction also revokes the Room-scoped
provider route. The ended Worker identity and retained Job records remain available for audit—they
are not presented as purged.

If a turn is still unsettled, ordinary cleanup refuses and shows the wait outcome. Use
`dorf job end first-job --interrupt` or `dorf worker end guide-worker --interrupt` only when you
intend to cancel that work. Cleanup failures remain visible and retryable.

## Failure and support boundaries

- Follow the safe action reported by a setup failure, then rerun `dorf setup`. `dorf doctor` is a
  separate diagnostic for an already configured core. Diagnostics are written under
  `$XDG_STATE_HOME/dorf/diagnostics/`, or
  `~/.local/state/dorf/diagnostics/`; review them before sharing because redacted host facts can
  still identify the machine. See [Setup support](support.md).
- Dorf will not modify a partially configured Incus installation whose resources it cannot identify
  as its own. Follow the observed-state diagnosis rather than deleting generic Incus resources.
- macOS, Windows, non-x86_64 hosts, remote Incus daemons, custom Room images, alternate VM backends,
  and harnesses other than Codex are not supported by `0.1.1`.
- This release uses local VMs. Work cannot continue while the host is powered off. A client or
  controller process may be replaced, and an existing Room can be recovered after the host returns.
- If the recorded Incus VM body or disk is gone, `worker recover` reports the Worker offline and
  Roomless. Durable identity and queued input remain inspectable, but executable conversation
  continuity cannot be restored; create a fresh Worker and Job.
- The runtime and Python SDK are experimental and do not yet provide a third-party compatibility
  promise. Use CLI help as the syntax authority: `dorf worker --help` and `dorf job --help`.
