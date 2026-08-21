# Support and diagnostics

The default supported deployment is x86_64 Linux. Ubuntu 24.04 is the proven clean-machine path.
Local profiles require a directly usable Incus daemon and KVM; cloud-only E2B deployments do not.
Other Linux distributions are capability-compatible only after their operator supplies the host
requirements selected during setup.

E2B requires an exact qualified template and a stable deployment-owned HTTPS Provider Gateway route.
Guided setup supports a named Cloudflare Tunnel; any operator-owned route satisfying the same exact
HTTPS `/v1` contract is valid. Disposable Quick Tunnels are proof-only. macOS, Windows, ARM, remote
Incus daemons, and host Docker-socket isolation are not supported. Custom Sandbox artifacts may be
admitted through an explicitly created and functionally verified profile, but carry no Dorf release
provenance.

Run the Go CLI's direct diagnostic boundary:

```bash
dorf doctor --provider NAME --profile SANDBOX_PROFILE
```

For a managed repository, also pass `--repo`, `--github-repo`, `--github-installation`, and `--base`.
Output is bounded JSON. Each item is `ready` or `failed` and
contains one concrete repair. Omit `--profile` to use the verified default. Dorf reports PostgreSQL,
Absurd, queue, selected Sandbox profile and its base verification,
Provider Gateway, repository reachability, and GitHub authority separately. Incus checks its command,
access, network, and credential-free image; E2B checks its exact profile configuration and host-only
API key.

Ownership guide:

- a minimal Incus command failing outside Dorf is an Incus or host-distribution problem;
- an E2B API or provider-resource failure outside Dorf is an E2B account, network, or service
  problem;
- the official image failing credential or Harness checks is a Dorf image or Harness compatibility
  problem;
- broker authentication failing independently is Provider Gateway/upstream provider work;
- incorrect durable facts, duplicate effects, leaked secrets, or incomplete cleanup are Dorf bugs;
- absent KVM or disabled virtualization is host configuration;
- another OS or architecture is unsupported, not silently equivalent.

Never attach Provider Gateway state, credentials, environment dumps, or Harness transcript contents
to a report. `dorf inspect JOB` contains stable IDs and bounded observed facts suitable for triage.
