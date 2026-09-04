# D044: Missing repository authority pauses inside the original delegation

- **Applicability:** historical
- **Areas:** workflows, github
- **Read when:** Reviewing the former pause-and-resume flow for missing GitHub repository authority.
- **Decision history:** Superseded by D047's explicit Go readiness/admission boundary — 2026-08-08
- **Decision:** When the exact issue-backed admission proof receives GitHub's not-found response
  before it can resolve the target branch, treat that one result as absent repository selection on
  the already configured Dorf GitHub App installation. Retain one deterministic, non-secret
  admission attempt keyed by the original command, local checkout, exact GitHub repository and
  GitHub App installation ID, starting commit, branch, issue, provider, and model inputs;
  create no Job, branch, Room, route, or AFK reservation. Open the installation's GitHub settings
  page with an attention item that accurately describes the persistent repository-wide grant and
  configured metadata-read, issues-read, contents-write, and pull-requests-write permissions.
  Observe only when branch authority appears through that same installation, then record idempotent
  approval and rerun the complete exact admission proof against the pinned repository and
  installation identities. The attempt expires after one hour, and decline or expiry is terminal
  for that generation; a later explicit retry creates a new generation without erasing the terminal
  record. The first coding Job reservation consumes approval and records admission in the same
  transaction so retries or a replaced controller process cannot create another Job.
- **Why:** Repository selection is one important, actionable authority decision that cannot safely
  be automated or replaced by a generic setup error. Keeping it inside the pinned delegation lets
  the owner approve once in GitHub while Dorf remains responsible for context retention, readiness,
  and continuation. The selected-repository grant persists beyond this delegation until a GitHub
  owner changes it; persisting only its non-secret installation ID, never its token or private key,
  keeps retained intent exact without turning durable workflow state into credential storage.
- **Compatibility:** Pending-attempt schema, expiry, failure code, installation URL, polling cadence,
  and CLI rendering are internal alpha surfaces. Other admission failures remain ordinary repair
  results; this does not introduce a general approval or workflow engine.
- **Reconsider when:** GitHub exposes a narrower repository-access callback than authority polling,
  an organization-request flow needs a distinct approval state, or a second concrete authority
  interruption proves a smaller shared primitive without leaking workflow policy into the runtime.
