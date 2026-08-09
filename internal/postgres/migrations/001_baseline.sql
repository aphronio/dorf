-- Greenfield Dorf product schema. Absurd 0.5.0 owns task attempts, claims,
-- checkpoints, waits, events, retries, cancellation, and task results in its
-- own schema; none of those mechanics are mirrored here.
create schema dorf;

create table dorf.schema_migrations (
    name text primary key,
    applied_at timestamptz not null default clock_timestamp()
);

create table dorf.jobs (
    id text primary key,
    admission_key text not null unique,
    goal text not null check (length(trim(goal)) > 0),
    repository text not null check (length(trim(repository)) > 0),
    revision text not null check (revision ~ '^[0-9a-f]{40}([0-9a-f]{24})?$'),
    starting_revision text not null check (starting_revision ~ '^[0-9a-f]{40}([0-9a-f]{24})?$'),
    branch text not null check (length(trim(branch)) > 0),
    github_repository text,
    github_installation_id text,
    base_branch text,
    publication_task_id text unique,
    provider_connection text not null check (length(trim(provider_connection)) > 0),
    provider_gateway_state text,
    model text not null check (length(trim(model)) > 0),
    reasoning_effort text not null check (reasoning_effort in ('low','medium','high','xhigh')),
    state text not null default 'admitted' check (state in ('admitted','running','observed','failed')),
    admission_open boolean not null default true,
    cleanup_state text not null default 'pending' check (cleanup_state in ('pending','scheduled','complete')),
    task_id text unique,
    cleanup_task_id text unique,
    workflow_phase text not null default 'setup' check (workflow_phase in (
        'setup','implementing','committing','checking','repairing',
        'review-planning','review-triage','reviewing','review-repairing',
        'ready','publishing','publication-blocked','published','blocked'
    )),
    repair_count integer not null default 0 check (repair_count between 0 and 1),
    workflow_attention text,
    setup_action_id text,
    review_repair_count integer not null default 0 check (review_repair_count between 0 and 1),
    review_repair_source_run_id text,
    cleanup_attention text,
    admitted_at timestamptz not null default clock_timestamp(),
    observed_at timestamptz,
    cleaned_at timestamptz,
    constraint jobs_github_authority_complete_check check (
        (github_repository is null and github_installation_id is null and base_branch is null) or
        (github_repository ~ '^[a-z0-9]([a-z0-9-]{0,37}[a-z0-9])?/[a-z0-9][a-z0-9_.-]*$' and
         github_installation_id ~ '^[1-9][0-9]*$' and length(base_branch) > 0 and base_branch <> branch)
    ),
    constraint jobs_github_authority_identity_key
        unique(id,github_repository,github_installation_id,base_branch,branch),
    constraint jobs_provider_gateway_state_absolute_check check (
        provider_gateway_state is null or
        (provider_gateway_state like '/%' and provider_gateway_state=btrim(provider_gateway_state) and
         provider_gateway_state !~ E'[\\n\\r]')
    )
);

create table dorf.job_messages (
    id text primary key,
    job_id text not null references dorf.jobs(id),
    caller_id text not null check (length(trim(caller_id)) > 0),
    sequence bigint not null check (sequence > 0),
    input text not null check (length(trim(input)) > 0),
    delivery_intent text not null default 'follow' check (delivery_intent in ('follow','steer')),
    steer_target_turn_id text,
    admitted_at timestamptz not null default clock_timestamp(),
    unique(job_id,caller_id),
    unique(job_id,sequence),
    constraint job_messages_delivery_target_check check (
        (delivery_intent='follow' and steer_target_turn_id is null) or
        (delivery_intent='steer' and steer_target_turn_id is not null)
    )
);

create table dorf.actions (
    id text primary key,
    job_id text not null references dorf.jobs(id),
    message_id text references dorf.job_messages(id),
    kind text not null check (kind in (
        'sandbox-create','repository-clone','repository-setup','repository-commit',
        'repository-push','github-pull-request','review-workspace-create','review-workspace-delete',
        'provider-route-create','codex-session-start','codex-turn-start',
        'provider-route-revoke','sandbox-delete'
    )),
    state text not null check (state in ('pending','succeeded','failed','uncertain')),
    external_id text,
    external_outcome text,
    attempts integer not null default 0 check (attempts >= 0),
    scope_key text not null default '',
    created_at timestamptz not null default clock_timestamp(),
    updated_at timestamptz not null default clock_timestamp(),
    constraint actions_turn_message_check check (
        (kind='codex-turn-start' and ((message_id is not null and scope_key='') or
                                     (message_id is null and scope_key<>''))) or
        (kind<>'codex-turn-start' and message_id is null)
    )
);
create unique index actions_one_unscoped_job_effect
    on dorf.actions(job_id,kind) where message_id is null and scope_key='';
create unique index actions_one_scoped_job_effect
    on dorf.actions(job_id,kind,scope_key) where message_id is null and scope_key<>'';
create unique index actions_one_message_effect
    on dorf.actions(message_id,kind) where message_id is not null;

create table dorf.sandboxes (
    job_id text primary key references dorf.jobs(id),
    action_id text not null unique references dorf.actions(id),
    incus_name text not null unique,
    state text not null check (state in ('pending','created','deleted')),
    observed_at timestamptz not null default clock_timestamp()
);

create table dorf.routes (
    job_id text primary key references dorf.jobs(id),
    action_id text not null unique references dorf.actions(id),
    route_id text not null unique,
    state text not null check (state in ('pending','active','revoked')),
    observed_at timestamptz not null default clock_timestamp()
);

create table dorf.sessions (
    job_id text primary key references dorf.jobs(id),
    action_id text not null unique references dorf.actions(id),
    native_session_id text not null unique,
    observed_at timestamptz not null default clock_timestamp()
);

create table dorf.agent_runs (
    id text primary key,
    job_id text not null references dorf.jobs(id),
    message_id text references dorf.job_messages(id),
    action_id text not null unique references dorf.actions(id),
    session_id text,
    state text not null default 'pending' check (state in ('pending','submitting','active','completed','failed','interrupted','uncertain')),
    baseline_native_turn_id text,
    native_turn_id text,
    native_outcome text check (native_outcome is null or native_outcome in ('completed','interrupted','failed')),
    attention text,
    role text not null check (role in ('implement','repair','review-triage','browser-ui','auth-authority','performance','critical-boundary')),
    revision text,
    capability text,
    workspace text,
    input_contract text,
    output_contract text,
    claim_evidence_id text,
    observed_evidence_id text,
    started_at timestamptz,
    finished_at timestamptz,
    input_tokens bigint not null default 0,
    cached_input_tokens bigint not null default 0,
    output_tokens bigint not null default 0,
    cost_microusd bigint not null default 0,
    usage_available boolean not null default false,
    yield_count integer not null default 0,
    observed_at timestamptz not null default clock_timestamp(),
    updated_at timestamptz not null default clock_timestamp(),
    constraint agent_runs_review_binding_check check (
        (role in ('implement','repair') and revision is null and capability is null and workspace is null) or
        (role in ('review-triage','browser-ui','auth-authority','performance','critical-boundary') and
         revision ~ '^[0-9a-f]{40}([0-9a-f]{24})?$' and capability='immutable-read-only' and
         length(workspace)>0 and length(input_contract)>0 and length(output_contract)>0)
    ),
    constraint agent_runs_review_measurements_check check (
        input_tokens>=0 and cached_input_tokens>=0 and output_tokens>=0 and cost_microusd>=0 and yield_count>=0 and
        (finished_at is null or (started_at is not null and finished_at>=started_at))
    )
);
create unique index agent_runs_one_message on dorf.agent_runs(message_id) where message_id is not null;
create unique index agent_runs_one_revision_role on dorf.agent_runs(job_id,revision,role) where revision is not null;
create unique index agent_runs_one_review_native_session on dorf.agent_runs(session_id) where revision is not null and session_id is not null;

create table dorf.revisions (
    job_id text not null references dorf.jobs(id),
    oid text not null check (oid ~ '^[0-9a-f]{40}([0-9a-f]{24})?$'),
    parent_oid text check (parent_oid is null or parent_oid ~ '^[0-9a-f]{40}([0-9a-f]{24})?$'),
    tree_oid text check (tree_oid is null or tree_oid ~ '^[0-9a-f]{40}([0-9a-f]{24})?$'),
    branch text not null,
    generation integer not null check (generation>=0),
    action_id text references dorf.actions(id),
    observed_at timestamptz not null default clock_timestamp(),
    primary key(job_id,oid),
    unique(job_id,generation)
);

create table dorf.repository_commands (
    job_id text not null references dorf.jobs(id),
    name text not null check (name in ('prepare','check','smoke')),
    command text not null check (length(command)>0),
    starting_revision text not null check (starting_revision ~ '^[0-9a-f]{40}([0-9a-f]{24})?$'),
    primary key(job_id,name)
);

create table dorf.checks (
    id text primary key,
    job_id text not null references dorf.jobs(id),
    name text not null check (name in ('check','smoke')),
    command text not null check (length(command)>0),
    revision text not null check (revision ~ '^[0-9a-f]{40}([0-9a-f]{24})?$'),
    state text not null check (state in ('running','passed','failed')),
    exit_code integer,
    evidence_id text,
    attention text,
    started_at timestamptz,
    finished_at timestamptz,
    unique(job_id,revision,name)
);

create table dorf.evidence (
    id text primary key,
    job_id text not null references dorf.jobs(id),
    digest text not null check (digest ~ '^[0-9a-f]{64}$'),
    byte_size bigint not null check (byte_size>=0),
    media_type text not null check (length(media_type)>0),
    producer text not null check (length(producer)>0),
    provenance text not null check (provenance in ('observed','claim')),
    kind text not null check (length(kind)>0),
    action_id text references dorf.actions(id),
    check_id text references dorf.checks(id),
    revision text check (revision is null or revision ~ '^[0-9a-f]{40}([0-9a-f]{24})?$'),
    started_at timestamptz not null,
    finished_at timestamptz not null,
    created_at timestamptz not null default clock_timestamp(),
    check (check_id is null or action_id is null),
    check (finished_at>=started_at)
);
alter table dorf.checks add constraint checks_evidence_id_fkey foreign key(evidence_id) references dorf.evidence(id);
alter table dorf.agent_runs add constraint agent_runs_claim_evidence_id_fkey foreign key(claim_evidence_id) references dorf.evidence(id);
alter table dorf.agent_runs add constraint agent_runs_observed_evidence_id_fkey foreign key(observed_evidence_id) references dorf.evidence(id);

create table dorf.review_plans (
    job_id text not null references dorf.jobs(id),
    revision text not null check (revision ~ '^[0-9a-f]{40}([0-9a-f]{24})?$'),
    state text not null check (state in ('pending','triage-pending','final','blocked')),
    facts jsonb,
    initial_policy jsonb,
    final_plan jsonb,
    policy_digest text check (policy_digest is null or policy_digest ~ '^[0-9a-f]{64}$'),
    triage_run_id text,
    triage_rationale text,
    created_at timestamptz not null default clock_timestamp(),
    finalized_at timestamptz,
    primary key(job_id,revision)
);

create table dorf.review_resources (
    run_id text primary key references dorf.agent_runs(id),
    job_id text not null references dorf.jobs(id),
    revision text not null check (revision ~ '^[0-9a-f]{40}([0-9a-f]{24})?$'),
    sandbox_name text not null unique,
    ownership_nonce text not null unique check (ownership_nonce ~ '^[0-9a-f]{64}$'),
    submission_nonce text not null unique check (submission_nonce ~ '^[0-9a-f]{64}$'),
    input_digest text not null check (input_digest ~ '^[0-9a-f]{64}$'),
    sandbox_create_action_id text not null unique references dorf.actions(id),
    route_create_action_id text not null unique references dorf.actions(id),
    materialize_action_id text not null unique references dorf.actions(id),
    route_revoke_action_id text not null unique references dorf.actions(id),
    sandbox_delete_action_id text not null unique references dorf.actions(id),
    sandbox_state text not null default 'pending' check (sandbox_state in ('pending','created','deleted')),
    route_state text not null default 'pending' check (route_state in ('pending','active','revoked')),
    checkout_state text not null default 'pending' check (checkout_state in ('pending','verified')),
    post_review_state text not null default 'pending' check (post_review_state in ('pending','verified')),
    route_id text unique,
    app_server_id text unique,
    revision_tree text check (revision_tree is null or revision_tree ~ '^[0-9a-f]{40}([0-9a-f]{24})?$'),
    checkout_verified_at timestamptz,
    post_review_verified_at timestamptz,
    route_revoked_at timestamptz,
    sandbox_deleted_at timestamptz
);

create table dorf.review_findings (
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

alter table dorf.jobs add constraint jobs_setup_action_id_fkey foreign key(setup_action_id) references dorf.actions(id);
alter table dorf.jobs add constraint jobs_review_repair_source_run_id_fkey foreign key(review_repair_source_run_id) references dorf.agent_runs(id);
alter table dorf.review_plans add constraint review_plans_triage_run_id_fkey foreign key(triage_run_id) references dorf.agent_runs(id);

create table dorf.github_proposals (
    job_id text primary key references dorf.jobs(id),
    repository text not null,
    installation_id text not null,
    base_branch text not null,
    head_branch text not null,
    pr_number bigint not null check (pr_number>0),
    pr_url text not null check (length(pr_url)>0),
    proposed_revision text not null check (proposed_revision ~ '^[0-9a-f]{40}([0-9a-f]{24})?$'),
    observed_remote_head text not null check (observed_remote_head ~ '^[0-9a-f]{40}([0-9a-f]{24})?$'),
    body_digest text not null check (body_digest ~ '^[0-9a-f]{64}$'),
    observed_at timestamptz not null default clock_timestamp(),
    foreign key(job_id,repository,installation_id,base_branch,head_branch)
        references dorf.jobs(id,github_repository,github_installation_id,base_branch,branch)
);
create unique index github_proposals_exact_outcome_identity
    on dorf.github_proposals(job_id,repository,installation_id,base_branch,head_branch,pr_number,proposed_revision);

create table dorf.job_outcomes (
    job_id text primary key references dorf.jobs(id),
    outcome text not null check (outcome in ('accepted','rejected','abandoned')),
    repository text not null,
    installation_id text not null,
    base_branch text not null,
    head_branch text not null,
    pr_number bigint not null check (pr_number>0),
    pr_url text not null check (length(pr_url)>0),
    proposed_revision text not null check (proposed_revision ~ '^[0-9a-f]{40}([0-9a-f]{24})?$'),
    observed_head text not null check (observed_head ~ '^[0-9a-f]{40}([0-9a-f]{24})?$'),
    observed_state text not null check (observed_state in ('open','closed')),
    observed_merged boolean not null,
    merge_commit_oid text check (merge_commit_oid is null or merge_commit_oid ~ '^[0-9a-f]{40}([0-9a-f]{24})?$'),
    observed_at timestamptz not null default clock_timestamp(),
    foreign key(job_id,repository,installation_id,base_branch,head_branch,pr_number,proposed_revision)
        references dorf.github_proposals(job_id,repository,installation_id,base_branch,head_branch,pr_number,proposed_revision),
    check (
        (outcome='accepted' and observed_state='closed' and observed_merged and merge_commit_oid is not null) or
        (outcome='rejected' and observed_state='closed' and not observed_merged and merge_commit_oid is null) or
        (outcome='abandoned' and not observed_merged and merge_commit_oid is null)
    )
);

comment on schema dorf is 'Dorf-owned product facts; Absurd execution state remains in schema absurd';
comment on table dorf.job_messages is 'Immutable client input and Job-local admission order; follow is FIFO and steer is an explicit priority lane';
comment on table dorf.agent_runs is 'Harness acceptance bindings and native outcome evidence only; Codex owns transcript and context';
comment on table dorf.evidence is 'Immutable content-addressed Evidence references; bytes live in deployment-owned storage';
comment on table dorf.review_resources is 'One Dorf-owned isolated Sandbox and provider route fact set per review Role';
comment on table dorf.github_proposals is 'One exact-Revision GitHub proposal projection per Job';
comment on table dorf.job_outcomes is 'Immutable Job outcome bound to the retained GitHub proposal';

insert into dorf.schema_migrations(name) values ('001_baseline.sql');
