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
    provider_connection text not null check (length(trim(provider_connection)) > 0),
    model text not null check (length(trim(model)) > 0),
    reasoning_effort text not null check (reasoning_effort in ('low','medium','high','xhigh')),
    admission_open boolean not null default true,
    cleanup_state text not null default 'pending' check (cleanup_state in ('pending','scheduled','complete')),
    task_id text unique,
    cleanup_task_id text unique,
    workflow_phase text not null default 'setup' check (workflow_phase in (
        'setup','implementing','checking',
        'review-planning','reviewing','review-feedback',
        'ready','publishing','publication-blocked','published','blocked'
    )),
    workflow_attention text,
    setup_action_id text,
    cleanup_attention text,
    admitted_at timestamptz not null default clock_timestamp(),
    cleaned_at timestamptz,
    constraint jobs_github_authority_complete_check check (
        (github_repository is null and github_installation_id is null and base_branch is null) or
        (github_repository ~ '^[a-z0-9]([a-z0-9-]{0,37}[a-z0-9])?/[a-z0-9][a-z0-9_.-]*$' and
         github_installation_id ~ '^[1-9][0-9]*$' and length(base_branch) > 0 and base_branch <> branch)
    ),
    constraint jobs_github_authority_identity_key
        unique(id,github_repository,github_installation_id,base_branch,branch)
);

create table dorf.job_messages (
    id text primary key,
    job_id text not null references dorf.jobs(id),
    from_kind text not null check (from_kind in ('human','agent','workflow')),
    from_id text not null check (length(trim(from_id)) > 0),
    sequence bigint not null check (sequence > 0),
    input text not null check (length(trim(input)) > 0),
    delivery_intent text not null default 'follow' check (delivery_intent in ('follow','steer')),
    steer_target_turn_id text,
    admitted_at timestamptz not null default clock_timestamp(),
    unique(job_id,from_kind,from_id),
    unique(job_id,sequence),
    unique(job_id,id),
    constraint job_messages_delivery_target_check check (
        (delivery_intent='follow' and steer_target_turn_id is null) or
        (delivery_intent='steer' and steer_target_turn_id is not null)
    )
);

create table dorf.actions (
    id text primary key,
    job_id text not null references dorf.jobs(id),
    kind text not null check (kind in (
        'sandbox-create','repository-clone','repository-setup',
        'repository-push','github-pull-request','review-checkout',
        'provider-route-create','provider-route-revoke','sandbox-delete'
    )),
    state text not null check (state in ('pending','succeeded','failed','uncertain')),
    external_id text,
    external_outcome text,
    scope_key text not null default '',
    created_at timestamptz not null default clock_timestamp()
);
create unique index actions_one_unscoped_job_effect
    on dorf.actions(job_id,kind) where scope_key='';
create unique index actions_one_scoped_job_effect
    on dorf.actions(job_id,kind,scope_key) where scope_key<>'';

create table dorf.sandboxes (
    id text primary key,
    job_id text not null references dorf.jobs(id),
    ownership_nonce text not null unique check (ownership_nonce ~ '^[0-9a-f]{64}$'),
    unique(job_id,id)
);
create index sandboxes_by_job on dorf.sandboxes(job_id,id);

create table dorf.agent_runs (
    id text primary key,
    job_id text not null references dorf.jobs(id),
    message_id text not null unique,
    state text not null default 'pending' check (state in ('pending','submitting','active','completed','failed','interrupted','uncertain')),
    harness text,
    thread_id text,
    baseline_turn_id text,
    turn_id text,
    turn_outcome text check (turn_outcome is null or turn_outcome in ('completed','interrupted','failed')),
    attention text,
    role text not null check (role in ('implement','general','browser-ui','auth-authority','performance','critical-boundary')),
    revision text,
    capability text,
    sandbox_id text not null references dorf.sandboxes(id),
    submission_nonce text unique,
    started_at timestamptz,
    finished_at timestamptz,
    constraint agent_runs_harness_thread_check check (
        (harness is null or length(trim(harness)) > 0) and
        (thread_id is null or (harness is not null and length(trim(thread_id)) > 0))
    ),
    constraint agent_runs_turn_binding_check check (
        turn_id is null or thread_id is not null
    ),
    constraint agent_runs_turn_outcome_binding_check check (
        turn_outcome is null or turn_id is not null
    ),
    constraint agent_runs_review_binding_check check (
        (role='implement' and revision is null and capability is null and submission_nonce is null) or
        (role in ('general','browser-ui','auth-authority','performance','critical-boundary') and
         revision ~ '^[0-9a-f]{40}([0-9a-f]{24})?$' and capability='immutable-read-only' and
         submission_nonce ~ '^[0-9a-f]{64}$')
    ),
    constraint agent_runs_timestamps_check check (
        (finished_at is null or (started_at is not null and finished_at>=started_at))
    ),
    foreign key(job_id,message_id) references dorf.job_messages(job_id,id),
    foreign key(job_id,sandbox_id) references dorf.sandboxes(job_id,id)
);
create unique index agent_runs_one_revision_role on dorf.agent_runs(job_id,revision,role) where revision is not null;

create table dorf.revisions (
    job_id text not null references dorf.jobs(id),
    oid text not null check (oid ~ '^[0-9a-f]{40}([0-9a-f]{24})?$'),
    comparison_base_oid text check (comparison_base_oid is null or comparison_base_oid ~ '^[0-9a-f]{40}([0-9a-f]{24})?$'),
    tree_oid text check (tree_oid is null or tree_oid ~ '^[0-9a-f]{40}([0-9a-f]{24})?$'),
    branch text not null,
    generation integer not null check (generation>=0),
    evidence_id text unique,
    observed_at timestamptz not null default clock_timestamp(),
    primary key(job_id,oid),
    unique(job_id,generation),
    check (
        (generation=0 and comparison_base_oid is null and tree_oid is null and evidence_id is null) or
        (generation>0 and comparison_base_oid is not null and tree_oid is not null and evidence_id is not null)
    )
);

create table dorf.repository_commands (
    job_id text not null references dorf.jobs(id),
    name text not null check (name in ('prepare','check','smoke')),
    command text not null check (length(command)>0),
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
    kind text not null check (length(kind)>0),
    action_id text references dorf.actions(id),
    check_id text references dorf.checks(id),
    agent_run_id text references dorf.agent_runs(id),
    revision text check (revision is null or revision ~ '^[0-9a-f]{40}([0-9a-f]{24})?$'),
    started_at timestamptz not null,
    finished_at timestamptz not null,
    created_at timestamptz not null default clock_timestamp(),
    check (num_nonnulls(action_id,check_id,agent_run_id)<=1),
    check (finished_at>=started_at)
);
create unique index evidence_one_agent_run on dorf.evidence(agent_run_id)
    where agent_run_id is not null;

alter table dorf.revisions
    add constraint revisions_evidence_fk foreign key(evidence_id) references dorf.evidence(id);
alter table dorf.checks add constraint checks_evidence_id_fkey foreign key(evidence_id) references dorf.evidence(id);

create table dorf.review_plans (
    job_id text not null references dorf.jobs(id),
    revision text not null check (revision ~ '^[0-9a-f]{40}([0-9a-f]{24})?$'),
    state text not null check (state in ('pending','final')),
    facts jsonb,
    plan jsonb,
    policy_digest text check (policy_digest is null or policy_digest ~ '^[0-9a-f]{64}$'),
    created_at timestamptz not null default clock_timestamp(),
    finalized_at timestamptz,
    primary key(job_id,revision)
);

create view dorf.review_run_projection as
select
    ar.id,
    ar.job_id,
    ar.message_id,
    ar.state,
    coalesce(ar.harness,'') as harness,
    coalesce(ar.thread_id,'') as thread_id,
    (ar.baseline_turn_id is not null)::boolean as baseline_recorded,
    coalesce(ar.baseline_turn_id,'') as baseline_turn_id,
    coalesce(ar.turn_id,'') as turn_id,
    coalesce(ar.turn_outcome,'') as turn_outcome,
    coalesce(ar.attention,'') as attention,
    ar.role,
    coalesce(ar.revision,'') as revision,
    coalesce(ar.capability,'') as capability,
    ar.started_at,
    ar.finished_at,
    request.from_kind as request_from_kind,
    request.from_id as request_from_id,
    request.sequence as request_sequence,
    request.input as request_input,
    request.delivery_intent as request_delivery_intent,
    coalesce(request.steer_target_turn_id,'') as request_target_turn_id,
    ar.sandbox_id as sandbox_id,
    sandbox.ownership_nonce as ownership_nonce,
    coalesce(ar.submission_nonce,'') as submission_nonce
from dorf.agent_runs ar
join dorf.job_messages request on request.id=ar.message_id and request.job_id=ar.job_id
join dorf.sandboxes sandbox on sandbox.id=ar.sandbox_id;

alter table dorf.jobs add constraint jobs_setup_action_id_fkey foreign key(setup_action_id) references dorf.actions(id);

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
comment on table dorf.agent_runs is 'Harness Thread and Turn bindings plus lifecycle outcome; the harness owns transcript and context';
comment on table dorf.evidence is 'Immutable content-addressed Evidence references; bytes live in deployment-owned storage';
comment on table dorf.sandboxes is 'Job-owned isolated workstations used by one or more AgentRuns';
comment on table dorf.github_proposals is 'One exact-Revision GitHub proposal projection per Job';
comment on table dorf.job_outcomes is 'Immutable Job outcome bound to the retained GitHub proposal';

insert into dorf.schema_migrations(name) values ('001_baseline.sql');
