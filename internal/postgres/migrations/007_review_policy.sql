alter table dorf.jobs
    add column if not exists review_repair_count integer not null default 0,
    add column if not exists review_repair_source_run_id text;

alter table dorf.jobs drop constraint if exists jobs_review_repair_count_check;
alter table dorf.jobs add constraint jobs_review_repair_count_check
    check (review_repair_count between 0 and 1);
alter table dorf.jobs drop constraint if exists jobs_workflow_phase_check;
alter table dorf.jobs add constraint jobs_workflow_phase_check check (
    workflow_phase in (
        'setup','implementing','committing','checking','repairing',
        'review-planning','review-triage','reviewing','review-repairing',
        'ready','blocked'
    )
);

alter table dorf.actions drop constraint if exists actions_kind_check;
alter table dorf.actions add constraint actions_kind_check check (kind in (
    'sandbox-create','repository-clone','repository-setup','repository-commit',
    'review-workspace-create','review-workspace-delete',
    'provider-route-create','codex-session-start','codex-turn-start',
    'provider-route-revoke','sandbox-delete'
));
alter table dorf.actions drop constraint if exists actions_turn_message_check;
alter table dorf.actions add constraint actions_turn_message_check check (
    (kind = 'codex-turn-start' and (
        (message_id is not null and scope_key='') or
        (message_id is null and scope_key<>'')
    )) or (kind <> 'codex-turn-start' and message_id is null)
);

alter table dorf.agent_runs alter column message_id drop not null;
drop index if exists dorf.agent_runs_one_message;
create unique index agent_runs_one_message on dorf.agent_runs(message_id) where message_id is not null;
alter table dorf.agent_runs drop constraint if exists agent_runs_session_id_fkey;
alter table dorf.agent_runs
    add column if not exists revision text,
    add column if not exists capability text,
    add column if not exists workspace text,
    add column if not exists input_contract text,
    add column if not exists output_contract text,
    add column if not exists claim_evidence_id text,
    add column if not exists observed_evidence_id text,
    add column if not exists started_at timestamptz,
    add column if not exists finished_at timestamptz,
    add column if not exists input_tokens bigint not null default 0,
    add column if not exists cached_input_tokens bigint not null default 0,
    add column if not exists output_tokens bigint not null default 0,
    add column if not exists cost_microusd bigint not null default 0,
    add column if not exists usage_available boolean not null default false,
    add column if not exists yield_count integer not null default 0;
alter table dorf.agent_runs drop constraint if exists agent_runs_role_check;
alter table dorf.agent_runs add constraint agent_runs_role_check check (role in (
    'implement','repair','review-triage','browser-ui','auth-authority','performance','critical-boundary'
));
alter table dorf.agent_runs drop constraint if exists agent_runs_review_binding_check;
alter table dorf.agent_runs add constraint agent_runs_review_binding_check check (
    (role in ('implement','repair') and revision is null and capability is null and workspace is null) or
    (role in ('review-triage','browser-ui','auth-authority','performance','critical-boundary') and
        revision ~ '^[0-9a-f]{40}([0-9a-f]{24})?$' and
        capability='immutable-read-only' and length(workspace) > 0 and
        length(input_contract) > 0 and length(output_contract) > 0)
);
alter table dorf.agent_runs drop constraint if exists agent_runs_review_measurements_check;
alter table dorf.agent_runs add constraint agent_runs_review_measurements_check check (
    input_tokens >= 0 and cached_input_tokens >= 0 and output_tokens >= 0 and
    cost_microusd >= 0 and yield_count >= 0 and
    (finished_at is null or (started_at is not null and finished_at >= started_at))
);
create unique index if not exists agent_runs_one_revision_role
    on dorf.agent_runs(job_id,revision,role) where revision is not null;
create unique index if not exists agent_runs_one_review_native_session
    on dorf.agent_runs(session_id) where revision is not null and session_id is not null;

create table if not exists dorf.review_plans (
    job_id text not null references dorf.jobs(id),
    revision text not null check (revision ~ '^[0-9a-f]{40}([0-9a-f]{24})?$'),
    state text not null check (state in ('pending','triage-pending','final','blocked')),
    facts jsonb,
    initial_policy jsonb,
    final_plan jsonb,
    policy_digest text check (policy_digest is null or policy_digest ~ '^[0-9a-f]{64}$'),
    requested_roles jsonb not null default '[]'::jsonb,
    requested_by_run_id text references dorf.agent_runs(id),
    triage_run_id text,
    triage_rationale text,
    created_at timestamptz not null default clock_timestamp(),
    finalized_at timestamptz,
    primary key(job_id,revision)
);
comment on table dorf.review_plans is
    'Atomic deterministic review selection for one immutable Revision; no-review is an explicit final decision';

create table if not exists dorf.review_workspaces (
    run_id text primary key references dorf.agent_runs(id),
    job_id text not null references dorf.jobs(id),
    revision text not null,
    path text not null unique,
    create_action_id text not null unique references dorf.actions(id),
    delete_action_id text not null unique references dorf.actions(id),
    state text not null default 'pending' check (state in ('pending','created','deleted')),
    created_at timestamptz,
    deleted_at timestamptz
);
comment on table dorf.review_workspaces is
    'Exact detached immutable inputs and independently retryable cleanup for review AgentRuns';

create table if not exists dorf.review_findings (
    run_id text primary key references dorf.agent_runs(id),
    job_id text not null references dorf.jobs(id),
    revision text not null,
    role text not null,
    material boolean not null,
    summary text not null,
    rationale text not null,
    affected_roles jsonb not null default '[]'::jsonb,
    affected_checks jsonb not null default '[]'::jsonb,
    evidence_id text not null unique references dorf.evidence(id),
    adjudication text not null default 'not-needed' check (adjudication in ('not-needed','pending','accepted','rejected')),
    stale boolean not null default false,
    recorded_at timestamptz not null default clock_timestamp()
);
comment on table dorf.review_findings is
    'Bounded reviewer claims; they never satisfy repository Checks';

alter table dorf.agent_runs drop constraint if exists agent_runs_claim_evidence_id_fkey;
alter table dorf.agent_runs add constraint agent_runs_claim_evidence_id_fkey
    foreign key(claim_evidence_id) references dorf.evidence(id);
alter table dorf.agent_runs drop constraint if exists agent_runs_observed_evidence_id_fkey;
alter table dorf.agent_runs add constraint agent_runs_observed_evidence_id_fkey
    foreign key(observed_evidence_id) references dorf.evidence(id);

alter table dorf.jobs drop constraint if exists jobs_review_repair_source_run_id_fkey;
alter table dorf.jobs add constraint jobs_review_repair_source_run_id_fkey
    foreign key(review_repair_source_run_id) references dorf.agent_runs(id);
alter table dorf.review_plans drop constraint if exists review_plans_triage_run_id_fkey;
alter table dorf.review_plans add constraint review_plans_triage_run_id_fkey
    foreign key(triage_run_id) references dorf.agent_runs(id);
