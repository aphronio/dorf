# Remote Control API Design

This document records the accepted design direction for Dorf's remote client boundary. It is an
implementation authority, not an enduring project principle. The
[implementation slices](control-api-slices.md) record proof details. This document distinguishes the
delivered contract from accepted future surface. Once the work is complete, retain only the facts
that remain useful in the ordinary product, architecture, operator, and API authorities; archive or
delete this working design.

## Delivery status

Slices 1 and 2 are implemented and dogfood-proven: one CLI Client can enroll, connect to one
Deployment, authenticate, and operate the complete direct Job interaction loop over HTTPS. The
delivered HTTP and CLI surfaces are listed separately below. Job listing, workflow admission,
OpenAPI publication, and managed service packaging remain planned work.

## Outcome

A person or agent should be able to operate a self-hosted Dorf deployment without SSH, knowledge of
its database, or access to its provider and Harness credentials:

```text
person / agent
      |
      |  HTTPS + stable JSON
      v
one Dorf Deployment
      |
      +-- Profiles choose verified Harness + Sandbox combinations
      +-- Jobs retain intent, progress, Messages, and cleanup custody
      +-- Workers reconcile durable work against providers and Harnesses
```

The first useful client is still the Dorf CLI. The same API should also be comfortable for an agent
using the CLI with structured output or direct HTTP. Generated code follows planned OpenAPI
publication. MCP, A2A, and a matrix of hand-written SDKs are deliberately deferred.

## Decision summary

| Question | Decision |
| --- | --- |
| Remote transport | HTTPS with stable JSON; SSH is not a client transport. OpenAPI follows in Slice 4. |
| Client targeting | One configured Deployment; no named contexts in version one. |
| Long-running work | Job is the resource; do not expose a generic Task, Run, or Operation. |
| Job creation retries | The caller creates request identity before sending; the CLI hides it from humans. |
| Observation | Canonical snapshots, conditional GET, and snapshot SSE; no copied event store. |
| Messages | Existing follow and steer invariants; adapters do not redefine them. |
| Files | Exact Sandbox-level reads only; no Job nesting, uploads, listing, or arbitrary writes. |
| Authentication | Short-lived Enrollment creates one Client with an independently revocable opaque credential. |
| Agent access | Structured CLI and HTTP first; OpenAPI in Slice 4; no required agent-specific transport. |
| Deployment | Private API listener behind HTTPS ingress and a separate worker; managed packaging in Slice 4. |
| Expansion | Workflows follow the direct Job proof; UI, OIDC, contexts, SDK families, and webhooks wait. |

## Vocabulary

| Term | Meaning |
| --- | --- |
| Deployment | One self-hosted Dorf control plane, including its durable Jobs and configured Profiles. |
| Profile | An existing verified Harness and Sandbox combination inside the Deployment. |
| Principal | The person or automation identity whose authority is being used. |
| Client | One CLI installation or automation integration authenticated as a Principal. |
| Enrollment | Short-lived, one-time authority to register one Client. |
| Credential | A revocable secret held by one Client and presented to the API. |
| Job | The durable unit of accepted user intent and the API's long-running resource. |
| Message | Durable follow or steer input accepted for a Job's Agent. |

`Client` is an authentication identity, not a second control plane and not an execution Profile.
AgentRun, Thread, Turn, Action, task claim, provider ownership token, and Harness operation remain
internal recovery facts rather than resources a remote caller coordinates.

## One configured Deployment

The CLI connects to one Deployment:

```text
dorf connect https://dorf.example.com
dorf auth status
```

The saved client configuration contains only the normalized Deployment URL and its one client
credential in a dedicated owner-only file. Dorf already has a protected-file deployment pattern;
adding a cross-platform credential-store dependency before the remote boundary is proven would add
more portability behavior than this slice needs. An operating-system credential store may replace
the protected file when a real client environment earns that integration.

Version one has no named contexts, context switching, context merge rules, or execution defaults in
client configuration. A Deployment already owns many Profiles, Sandbox providers, Harnesses, and
Jobs. Adding another client-side deployment hierarchy would solve a problem we have not observed
and make scripts depend on hidden state. A future automation client may accept an explicit
Deployment URL and credential without adding persistent contexts.

If operating several Deployments becomes a real repeated need, add explicit one-command targeting
before considering persistent contexts.

## Resource model

The delivered API is resource-oriented HTTPS with purpose-built JSON representations:

```text
GET   /v1                                  deployment/version/capability discovery
POST  /v1/auth/enrollments/redeem          redeem one Enrollment
GET   /v1/me                               effective Principal and Client

POST  /v1/jobs                             admit or replay a direct Job
GET   /v1/jobs/{job}                       inspect one canonical Job snapshot
GET   /v1/jobs/{job}/watch                 observe newer canonical snapshots
POST  /v1/jobs/{job}/messages              follow or steer
GET   /v1/jobs/{job}/messages/{message}    inspect one Message delivery
POST  /v1/jobs/{job}/retries               retry eligible failed work
GET   /v1/sandboxes/{sandbox}/files?path=REPORT.md
GET   /v1/jobs/{job}/evidence              inspect verified Evidence metadata
PUT   /v1/jobs/{job}/cleanup               request cleanup idempotently
```

Later slices plan:

```text
GET   /v1/jobs
POST  /v1/workflows/coding/jobs
POST  /v1/workflows/codebase-investigation/jobs
```

The direct Job path is proven. Typed workflow admissions follow without turning workflow
identity into a generic plugin or DSL.

There is no public Task, Run, Operation, Thread, Turn, Action, provider, or Harness endpoint. A Job
is the one long-running resource. Retry, Message admission, exact file retrieval, and cleanup are
operations on resources Dorf already owns.

## Jobs and state

A Job representation is a purpose-built API contract rather than a serialization of PostgreSQL rows
or internal Go structs. It keeps independent facts independent:

```json
{
  "id": "job-...",
  "kind": "direct",
  "goal": "...",
  "profile": "default",
  "model": "...",
  "reasoning": "high",
  "initial_message_id": "message-...",
  "admission": { "open": true },
  "execution": { "state": "idle" },
  "attention": null,
  "cleanup": { "state": "not_requested" },
  "sandboxes": [
    { "id": "dorf-...", "name": "default" }
  ]
}
```

These meanings are not collapsed into one synthetic status. A successful direct Turn may leave its
Job open and idle. Attention does not mean cleanup was requested. A future workflow Outcome must not
itself mean cleanup completed.

When Job listing is added, its responses will be paginated from the start. JSON field names, enum
values, error codes, and exit behavior are compatibility surface; human CLI prose is not.

## Messages

Remote Message admission exposes the invariant Dorf semantics already owned by Core:

- follow is durable FIFO input, may queue early, retains the authoritative Thread, and creates a new
  Turn;
- steer targets the exact active Turn, has priority over queued follows, and never falls back to a
  new Turn;
- cleanup timing remains caller or workflow policy.

The API exposes Message identity, delivery state, and an available settled result, not AgentRun,
Harness Thread, or Turn IDs. After cleanup begins, retained delivery state remains readable while
Harness output is no longer fetched.

## Exact Sandbox files

File retrieval is a Sandbox-level read because the Sandbox owns the bytes:

```text
GET /v1/sandboxes/{sandbox}/files?path=REPORT.md
```

The Job snapshot tells the caller which Sandboxes the Job owns. The server then enforces exact
Sandbox custody and the existing safe relative-path and cleanup fence. The response is exact
`application/octet-stream` bytes with `Content-Length` and a SHA-256 `Content-Digest` that the CLI
verifies before writing.

Version one does not add file upload, arbitrary writes, listing, globbing, directory download,
archives, or server-side destination paths. A future write operation must be earned separately; it
must not be smuggled into the read endpoint.

Evidence retrieval returns only verified immutable metadata. It does not expose internal Action or
AgentRun identities or retained blob bytes, and a direct Job may truthfully return an empty list.

## Automatic retry identity

Every operation that may create durable state has a request identity known before the request is
sent. This closes the ordinary lost-response ambiguity:

```text
client sends request K
        |
        +-- server commits Job
        |
        `-- response is lost

client retries K  ----------> same Job, never a duplicate Job
```

Humans do not manage this key in the ordinary successful flow. The Dorf CLI creates it before Job
admission, Message admission, and explicit retry; it retries the same mutation once after an
ambiguous transport or server failure and reveals the key only if ambiguity remains. A raw HTTP
caller supplies `Idempotency-Key`. Reusing the key with the same request returns the same resource;
reusing it with materially different input returns a stable typed conflict. The server cannot safely
generate the only copy after admission: that copy is precisely what may be lost with the response.

## Observation, not a second event system

`GET /v1/jobs/{job}` is the canonical current snapshot and supports representation ETags. The
`watch` server-sent event stream delivers a newer complete snapshot when that representation
changes, plus keepalive, bounded reauthentication, and reconnect behavior.

The stream is a delivery optimization over authoritative facts, not a new durable event log. It may
coalesce intermediate projections. Reconnecting clients resume by re-reading the canonical Job.
Webhooks and an externally replayable event history are deferred until a real consumer requires
those different guarantees.

The observation CLI mirrors that separation incrementally:

- `job inspect --output json` emits one stable JSON document;
- `job watch --output jsonl` emits one snapshot per line;
- human output remains concise and situation-first;
- successful data goes to stdout, progress and diagnostics to stderr.

## Errors and recovery

Dorf-origin API failures use Problem Details with a stable Dorf code, retryability, and structured
details. Clients never need to parse its prose. An ingress or other non-Dorf failure may not be a
Problem response, so mutation retry also recognizes transport and HTTP server failure directly.
Message, steer, retry, file, Evidence, and idempotency failures have stable typed codes; Slice 4 will
publish the full catalog.

```json
{
  "type": "https://dorf.dev/problems/idempotency-conflict",
  "title": "Idempotency-Key is bound to different Job input",
  "status": 409,
  "code": "idempotency_conflict",
  "retryable": false,
  "details": {}
}
```

Use ordinary HTTP meaning: unauthenticated is `401`, authenticated but unauthorized is `403`, absent
is `404`, optimistic or request-identity conflict is `409` or `422` according to the actual contract,
rate limiting is `429`, and unexpected service failure is `5xx`. Partial success must remain
representable; for example, a retained outcome followed by cleanup scheduling failure cannot be
reduced to an unstructured error string.

## Authentication and enrollment

The smallest honest self-hosted boundary is HTTPS plus per-Client opaque bearer credentials:

```text
host-authorized operator
        |
        | creates short-lived, one-use Enrollment
        v
remote `dorf connect`
        |
        | redeems Enrollment over HTTPS
        v
one named Client + one revocable credential
```

Version one begins with one Deployment-operator Principal and permits multiple named Clients for
that Principal. Enrollment is short-lived, single-use, rate-limited, and grants exactly one Client.
The durable credential is high entropy, individually expiring and revocable. The Client generates
it before redemption, so an interrupted response can retry the same complete binding; Dorf stores
only its digest and never needs to return or recover plaintext. The API authenticates
`Authorization: Bearer`; credentials never appear in URLs, Job facts, Profiles, transcripts, logs,
or provider configuration.

This is deliberately not a permanent global API key, a Dorf password system, a Dorf-issued JWT, or
a home-grown OAuth provider. OIDC for company identities, workload identity for automation, and
sender-constrained credentials such as mTLS may be added later behind the same normalized Principal
and Client model when real deployments require them.

TLS terminates at an operator-owned HTTPS ingress or private-network service; `dorf serve` remains a
plain HTTP listener on loopback. Provider Gateway route credentials, AI connection credentials,
GitHub App credentials, Profile configuration, and trusted-proxy headers are not Dorf client
authentication. The operator must assign the control API and the Provider Gateway's
OpenAI-compatible `/v1` service separate HTTPS origins; that deployment rule is not inferred from a
shared hostname.

## CLI, developer, and agent experience

The delivered remote CLI vocabulary is:

```text
dorf connect https://dorf.example.com
dorf auth status

dorf run --goal-file goal.md --model MODEL
dorf job inspect JOB
dorf job watch JOB
dorf job watch --output jsonl JOB
dorf job message --input-file follow-up.md JOB
dorf job message --intent steer --input-file correction.md JOB
dorf job message inspect JOB MESSAGE
dorf job retry JOB
dorf job evidence JOB
dorf sandbox file get SANDBOX PATH --output DESTINATION
dorf job cleanup JOB
```

The deployment host creates and revokes remote Clients with `dorf client enroll` and
`dorf client revoke CLIENT_ID`; it keeps `dorf serve` on an exact private loopback address. There is
one saved Deployment and no named multi-Deployment switching. `dorf connect` accepts an Enrollment
interactively or through `--enrollment-file PATH|-` for non-interactive use.

The CLI creates mutation identity internally, retries the exact request once after a retryable
transport or HTTP server failure, and reveals the recovery key in human output only if ambiguity
remains. Structured mutation receipts include the request identity. Mutations return the effective
Deployment, canonical resource identity, and accepted state. Cleanup names one exact Job.

Later slices add `job list` and typed workflow admissions. `watch` is used instead of overloading
`inspect --follow`, because Follow already has a precise Message meaning.

Agents initially use the same CLI with structured output or call the described HTTP API directly
from code mode. OpenAPI publication remains Slice 4 work. MCP and A2A are not part of the first
transport: they would add another protocol and capability translation before the resource model has
been proven by real clients.

## Deployment boundary

The API process and durable worker are separate deployment responsibilities. They may be co-located,
but their responsibilities remain separate:

```text
HTTPS API                    worker
  authenticate                claim durable work
  validate/admit              reconcile providers/Harnesses
  read canonical state        recover after process loss
  return snapshots            execute requested cleanup
           \                 /
            PostgreSQL + Absurd
```

The API composes existing direct application handles; later typed workflow adapters use the same
boundary. It does not shell out to the Dorf CLI, expose PostgreSQL, hand out task claims, or expose
provider/Harness operations.

Slices 1 and 2 proved independent API and worker restart, plus watch reconnection after API loss,
with transient, separately supervised processes.
A supported managed lifecycle, diagnostics, and upgrade path remain Slice 4 work; current operator
limits are documented once in [Getting started](../getting-started.md#3-connect-one-remote-cli-client).
The Provider Gateway's HTTPS `/v1` route is a different authority on a different hostname from this
control API and must not be used as the Deployment URL.

## Explicitly deferred

- multiple saved Deployment contexts;
- browser UI and browser login;
- multi-user RBAC, teams, organizations, and OIDC;
- workload identity and mTLS;
- MCP, A2A, and hand-written SDK families;
- webhooks or a copied public event store;
- arbitrary Sandbox file writes, listing, or archives;
- public workflow registration, plugins, or a workflow DSL;
- cancellation semantics not already earned by Job cleanup;
- high-availability control-plane topology.

## Design influences

The resource and HTTP choices follow
[Fielding's REST dissertation](https://www.ics.uci.edu/~fielding/pubs/dissertation/top.htm),
[HTTP Semantics (RFC 9110)](https://www.rfc-editor.org/rfc/rfc9110),
[Problem Details (RFC 9457)](https://www.rfc-editor.org/rfc/rfc9457), and the
[Google API Improvement Proposals](https://google.aip.dev/). Retry identity follows the failure model
described by [Stripe](https://docs.stripe.com/api/idempotent_requests) and the
[AWS Builders' Library](https://aws.amazon.com/builders-library/making-retries-safe-with-idempotent-APIs/).
The OpenAPI description follows the [OpenAPI specification](https://spec.openapis.org/oas/latest.html),
and CLI behavior follows the [Command Line Interface Guidelines](https://clig.dev/).

Docker and Kubernetes contexts informed the decision not to add contexts before Dorf has an observed
multi-Deployment client need. Hyrum's Law informs the decision to keep internal recovery records out
of the public representation: every observable detail eventually becomes somebody's dependency.
