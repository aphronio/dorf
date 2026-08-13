# Support and diagnostics

Supported today: x86_64 Linux with a directly usable local Incus daemon. Ubuntu 24.04 is the proven
clean-machine path. Other Linux distributions are capability-compatible only after their operator
installs and initializes Incus. macOS, Windows, ARM, remote Incus daemons, custom Sandbox images,
and host Docker-socket isolation are not supported.

Run the Go CLI's direct diagnostic boundary:

```bash
dorf doctor --provider NAME
```

For a managed repository, also pass `--contract`, `--repo`, `--github-repo`,
`--github-installation`, and `--base`. Output is bounded JSON. Each item is `ready` or `failed` and
contains one concrete repair. Dorf reports PostgreSQL, Absurd, queue, Incus command/access/network,
credential-free image, Provider Gateway, repository contract, and GitHub authority separately.

Ownership guide:

- a minimal Incus command failing outside Dorf is an Incus or host-distribution problem;
- the official image failing credential or Harness checks is a Dorf image or Harness compatibility
  problem;
- broker authentication failing independently is Provider Gateway/upstream provider work;
- incorrect durable facts, duplicate effects, leaked secrets, or incomplete cleanup are Dorf bugs;
- absent KVM or disabled virtualization is host configuration;
- another OS or architecture is unsupported, not silently equivalent.

Never attach Provider Gateway state, credentials, environment dumps, or Harness transcript contents
to a report. `dorf inspect JOB` contains stable IDs and bounded observed facts suitable for triage.
