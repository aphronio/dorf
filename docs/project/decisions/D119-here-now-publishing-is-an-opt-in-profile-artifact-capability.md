# D119: here.now publishing is an opt-in Profile-artifact capability

- **Applicability:** current
- **Areas:** sandboxes, harnesses, deployment
- **Read when:** Changing here.now skill provisioning, publish credentials, output redaction, or optional Sandbox artifact contents.
- **Decision history:** Accepted after auditing here.now skill 1.29.0 and its complete bundled helpers at
  `heredotnow/skill` commit `8cf033ed53b82c0c67b16359c8c431f99e111d04` — 2026-09-12.
- **Decision:** Reuse the exact Sandbox Profile artifact boundary for optional here.now publishing.
  A custom Incus image build may opt in and must use a non-default alias; an ordinary explicitly
  selected Codex Profile then pins its resolved fingerprint. The official image and default Profile
  remain unchanged. Provisioning downloads the pinned upstream publish helper, verifies its recorded
  SHA-256, installs a narrower Dorf skill at Codex's native `/root/.codex/skills/here-now` discovery
  path, and records the exact parent fingerprint, Dorf commit, and source and adaptation digests in
  image metadata. Provisioning verifies that the exact selected parent artifact supplies the
  required `curl`, `file`, and `jq` binaries and fails rather than building an incomplete image. Pi
  does not discover this Codex
  skill. E2B remains excluded until its public HTTPS regression in issue 158 is repaired and the
  capability receives a provider proof.
- **Safety adaptation:** The upstream curl installer is not executed: it follows mutable `main`,
  targets Claude's home, and downloads an executable. The full upstream `publish.sh` and `drive.sh`
  were audited. Upstream publishing may retain anonymous claim capabilities in
  `.herenow/state.json`, prints claim URLs, can include raw API responses on errors, accepts an
  override API base, and ordinarily places the account key in curl arguments. Drive operations
  deliberately return share tokens and broad response bodies. Dorf therefore enables only the
  pinned publish helper behind a wrapper that refuses anonymous, environment, command-line, Drive,
  and alternate-base credentials; passes authorization to curl through a protected header file;
  restricts returned upload and finalize URLs to the documented authorities; contains all vendor
  streams; validates and emits only nonsecret publish metadata; requires Git to
  ignore local state; and forces the state directory and file to `0700` and `0600`. Shell tracing is
  disabled before secret handling. Drive, authentication, account administration, access mutation,
  purchases, and raw API use remain disabled. The pinned vendor helper is installed without execute
  permission so the redacting wrapper is the only normal entry point.
- **Authorization boundary:** Skill discovery never supplies publication authority. No image,
  Profile, Dorf worker environment, or repository contains a here.now key. Current named here.now
  API keys are account-wide; a publisher name is attribution, not server-enforced publish scope. A
  trusted coordinator may, after explicit task authorization, use the existing authenticated exact
  Sandbox file-write boundary to create `~/.herenow/credentials` as `0600` only in the selected
  coordinator Sandbox.
  The agent can access any credential placed in its Sandbox; this is therefore suitable only for a
  trusted coordinator, not an unprivileged worker. Dorf has no here.now secret store, credential
  broker, automatic renewal, account login, or publication Action.
- **Durability boundary:** A protected file survives application redeploys and restarts only while
  its retained Sandbox disk survives. Replacing or losing that Sandbox loses the file. A deployment
  secret source such as a narrowly mapped Dokploy secret can let the coordinator reprovision a new
  Sandbox, but adding an unused variable does nothing and mapping it into a general worker grants
  excessive authority. Loss of the Dokploy host also loses host-local secret state unless its
  database and volumes are backed up or an external secret manager or protected owner backup remains
  authoritative. The coordinator application owns that later retrieval and injection workflow;
  Dorf continues to own only exact file custody and Sandbox isolation.
- **Remaining integration:** A Dokploy variable alone is not an integration: the current Dorf
  Compose project has no here.now consumer or mapping, and deployment environment never flows into
  separate Sandboxes. The smallest next change belongs in the repository that defines the trusted
  coordinator service. It should map the protected source only into that service, create-only write
  the key through Dorf's exact file API for one authorized coordinator Sandbox, accept only wrapper
  output, and remove and observe the file after the bounded operation. Its synthetic recovery test
  should use a coordinator Sandbox, recreate it with neither file nor environment, reinject from the
  protected source, and repeat discovery plus a mocked publish. A separate unprivileged Sandbox must
  be unable to read the coordinator path and must have no key in its environment. Test application
  redeploy and same-VM restart as retained-disk cases; test VM replacement as reinjection; and test
  Dokploy-host replacement from a restored Dokploy database/volume backup or external secret source.
- **Proof:** Synthetic adapter tests place mock API-key, claim, upload, and finalize capability
  values in every upstream output channel and a permissive state file. They verify complete output
  redaction, allowlisted success metadata, sanitized failure, environment-key refusal, Git ignore
  enforcement, and repaired private modes, including when invoked with shell tracing. A fresh
  staging-root provisioning smoke verifies the exact upstream checksum, Codex discovery path,
  credential absence, and image metadata. A real here.now publish is deliberately not part of image
  construction or repository verification.
- **Why:** The exact Profile artifact is already Dorf's durable tool-provisioning unit. Using it
  avoids per-VM installation and does not revive the deleted generic capability matcher. The opt-in
  artifact prevents a vendor publishing instruction from appearing in unrelated or default agents.
- **Cost:** The optional artifact adds vendor code and network destinations whose upgrades require a
  fresh audit and pin. Base Profile verification does not yet prove here.now egress, discovery, or
  publication safety; operators must treat those as separate capability evidence.
- **Reconsider when:** A shipped workflow needs typed publishing authority, here.now offers a
  publish-scoped credential, E2B regains and proves compatible egress, or several optional artifact
  capabilities justify a general signed composition format.
