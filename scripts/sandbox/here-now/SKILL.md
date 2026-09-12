---
name: here-now
description: Publish an explicitly authorized static file or directory to a here.now account or workspace through Dorf's redacting helper. Requires a dedicated credential delivered only to a trusted coordinator.
---

# here.now publishing in Dorf

This is a safety-constrained adaptation of here.now skill version 1.29.0. The vendor publish helper
is pinned to `heredotnow/skill` commit `8cf033ed53b82c0c67b16359c8c431f99e111d04`.

Read the current [here.now documentation](https://here.now/docs) and
[OpenAPI document](https://here.now/openapi.json) before planning an operation. Current server
behavior is authoritative, but do not bypass this skill's narrower safety boundary.

## Authority and secrets

- Availability is not authorization. Publish only after the current task explicitly authorizes the
  exact content and destination account or workspace.
- A trusted coordinator must provision a dedicated key at `~/.herenow/credentials` as a regular
  `0600` file inside a coordinator-owned `0700` directory only for the bounded operation. Current
  here.now API keys remain account-wide; naming
  one for publishing does not reduce its server-side authority. Do not create an account, use email
  authentication endpoints, list or create account
  keys, ask the owner for credentials, or place keys in commands or environment variables.
- Never read, print, return, or log credentials, claim tokens or URLs, Drive tokens, presigned upload
  URLs, finalize URLs, raw API responses, or `.herenow/state.json`.
- Do not use anonymous publishing. Its claim URL is a bearer capability that this adaptation never
  exposes.
- Drive sharing, domain registration or purchase, billing, account administration, and access-policy
  mutation are not enabled by this capability.

## Publish

Use only the bundled wrapper; do not call the API, the pinned vendor helper, `npx skills add`, or the
curl installer directly:

```bash
/root/.codex/skills/here-now/scripts/publish.sh PATH --client codex/dorf
```

Add `--workspace WORKSPACE` only when the task names that workspace. The wrapper refuses command-line
or environment credentials, non-default API bases, Drive publishing, anonymous publishing, and Git
worktrees that do not ignore `.herenow/state.json`. It contains all vendor output and emits only the
validated site URL plus allowlisted nonsecret publish metadata.

If it fails, report the sanitized wrapper error and ask the trusted coordinator to inspect the
credential or egress boundary. Do not rerun with shell tracing or replace the wrapper with curl.
Publishing needs validated HTTPS access to `here.now` and the presigned
`*.r2.cloudflarestorage.com` upload host. A successful helper exit proves only that this publish
completed; it grants no continuing authority for another publish.
