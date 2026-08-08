# Revision-pinned Checks and Evidence

Repository verification is a Go-owned phase of the durable Job. The Sandbox clone reads the
repository-owned `.dorf.toml`, runs `commands.prepare` programmatically, commits through Git, and
runs declared `check` and `smoke` commands at one exact Revision. Dorf retains bounded command
output as content-addressed Evidence and independently rehashes it before readiness.

Changing the Revision invalidates only proof that depended on the old Revision. Agent prose,
review findings, native transcript items, and database rows without their retained artifact cannot
satisfy a Check.

This repository's contract is deliberately Go-first:

```toml
[commands]
prepare = "go mod download"
check = "go test ./... && go vet ./..."
smoke = "mkdir -p .dorf/bin && go build -o .dorf/bin/dorf ./cmd/dorf && .dorf/bin/dorf version && go version -m .dorf/bin/dorf"
```

Material selected-review findings return once to the original implementation Session. A repair
creates a new immutable Revision and reruns the policy/check obligations that depend on it. Review
AgentRuns remain claims; readiness comes from deterministic Checks and verified Evidence.

Inspect and rehash retained proof with:

```bash
dorf inspect JOB_ID
dorf evidence verify JOB_ID
```
