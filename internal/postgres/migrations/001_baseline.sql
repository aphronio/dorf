-- Greenfield Dorf product schema. Absurd 0.5.0 owns task attempts, claims,
-- checkpoints, waits, events, retries, cancellation, and task results in its
-- own schema; none of those mechanics are mirrored here.
create schema dorf;

create table dorf.schema_migrations (
    name text primary key,
    applied_at timestamptz not null default clock_timestamp()
);

create table dorf.sandbox_profiles (
    name text primary key check (name ~ '^[a-z][a-z0-9-]{0,62}$'),
    provider text not null check (provider in ('incus','e2b')),
    harness text not null check (harness in ('codex','pi')),
    artifact text not null check (length(trim(artifact)) > 0),
    incus_network text,
    incus_disk_size text,
    e2b_gateway_url text,
    e2b_sandbox_timeout_seconds bigint,
    e2b_allow_internet boolean,
    is_default boolean not null default false,
    created_at timestamptz not null default clock_timestamp(),
    check (
        (provider='incus' and incus_network is not null and incus_disk_size is not null and
         length(trim(incus_network)) > 0 and length(trim(incus_disk_size)) > 0 and
         e2b_gateway_url is null and e2b_sandbox_timeout_seconds is null and e2b_allow_internet is null) or
        (provider='e2b' and incus_network is null and incus_disk_size is null and
         e2b_gateway_url is not null and e2b_sandbox_timeout_seconds is not null and
         length(trim(e2b_gateway_url)) > 0 and e2b_sandbox_timeout_seconds > 0 and e2b_allow_internet is not null)
    )
);
create unique index sandbox_profiles_one_default on dorf.sandbox_profiles(is_default) where is_default;

create table dorf.sandbox_profile_verifications (
    profile_name text primary key references dorf.sandbox_profiles(name) on delete cascade,
    contract_version text not null check (length(trim(contract_version)) > 0),
    sandbox_id text not null unique check (length(trim(sandbox_id)) > 0),
    ownership_nonce text not null unique check (ownership_nonce ~ '^[0-9a-f]{64}$'),
    harness_version text,
    attempted_at timestamptz not null default clock_timestamp(),
    probe_completed_at timestamptz,
    cleaned_at timestamptz,
    last_error text,
    check (probe_completed_at is null or (harness_version is not null and length(trim(harness_version)) > 0)),
    check (cleaned_at is null or cleaned_at >= coalesce(probe_completed_at,attempted_at))
);

create table dorf.jobs (
    id text primary key,
    admission_key text not null unique,
    workflow_name text not null check (workflow_name in ('coding-to-proposal','codebase-investigation')),
    workflow_revision text not null check (length(trim(workflow_revision)) > 0),
    goal text not null check (length(trim(goal)) > 0),
    sandbox_profile text not null references dorf.sandbox_profiles(name),
    provider_connection text not null check (length(trim(provider_connection)) > 0),
    model text not null check (length(trim(model)) > 0),
    reasoning_effort text not null check (reasoning_effort in ('low','medium','high','xhigh')),
    admission_open boolean not null default true,
    cleanup_state text not null default 'pending' check (cleanup_state in ('pending','scheduled','complete')),
    workflow_attention text,
    workflow_attention_source text,
    workflow_attention_at timestamptz,
    cleanup_attention text,
    admitted_at timestamptz not null default clock_timestamp(),
    cleaned_at timestamptz,
    constraint jobs_workflow_attention_check check (
        num_nonnulls(workflow_attention,workflow_attention_source,workflow_attention_at) in (0,3)
    ),
    unique(id,workflow_name)
);

create table dorf.coding_to_proposal_inputs (
    job_id text primary key,
    workflow_name text not null check (workflow_name='coding-to-proposal'),
    repository text not null check (length(trim(repository))>0),
    starting_revision text not null check (starting_revision ~ '^[0-9a-f]{40}([0-9a-f]{24})?$'),
    revision text not null check (revision ~ '^[0-9a-f]{40}([0-9a-f]{24})?$'),
    branch text not null check (length(trim(branch))>0),
    github_repository text not null check (
        github_repository ~ '^[a-z0-9]([a-z0-9-]{0,37}[a-z0-9])?/[a-z0-9][a-z0-9_.-]*$'
    ),
    github_installation_id text not null check (github_installation_id ~ '^[1-9][0-9]*$'),
    base_branch text not null check (length(trim(base_branch))>0 and base_branch<>branch),
    foreign key(job_id,workflow_name) references dorf.jobs(id,workflow_name)
);

create table dorf.job_tasks (
    job_id text not null references dorf.jobs(id),
    sequence bigint not null check (sequence > 0),
    task_id text not null unique check (length(trim(task_id)) > 0),
    task_name text not null check (length(trim(task_name)) > 0),
    attached_at timestamptz not null default clock_timestamp(),
    primary key (job_id,sequence)
);

create table dorf.codebase_investigation_sources (
    job_id text primary key,
    workflow_name text not null check (workflow_name='codebase-investigation'),
    kind text not null check (kind in ('remote','git-bundle')),
    repository text not null,
    revision text not null check (revision ~ '^[0-9a-f]{40}([0-9a-f]{24})?$'),
    bundle_digest text,
    bundle_byte_size bigint,
    check (
        (kind='remote' and length(trim(repository))>0 and bundle_digest is null and bundle_byte_size is null) or
        (kind='git-bundle' and length(trim(repository))=0 and bundle_digest is not null and bundle_byte_size is not null and
         bundle_digest ~ '^[0-9a-f]{64}$' and bundle_byte_size>0)
    ),
    foreign key(job_id,workflow_name) references dorf.jobs(id,workflow_name)
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
        'sandbox-create','repository-clone','repository-restore',
        'repository-push','github-pull-request','review-checkout',
        'provider-route-create','provider-route-revoke','sandbox-delete'
    )),
    state text not null check (state in ('unsettled','succeeded','failed')),
    scope_key text not null default '',
    created_at timestamptz not null default clock_timestamp(),
    settled_at timestamptz,
    check (
        (state='unsettled' and settled_at is null) or
        (state in ('succeeded','failed') and settled_at is not null and settled_at>=created_at)
    )
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
    role text not null check (role in ('implement','investigate','general','browser-ui','auth-authority','performance','critical-boundary')),
    input_revision text check (input_revision is null or input_revision ~ '^[0-9a-f]{40}([0-9a-f]{24})?$'),
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
    constraint agent_runs_role_binding_check check (
        (role='implement' and capability is null and submission_nonce is null) or
        (role='investigate' and input_revision is not null and capability='repository-read-report' and submission_nonce is null) or
        (role in ('general','browser-ui','auth-authority','performance','critical-boundary') and
         input_revision is not null and capability='immutable-read-only' and
         submission_nonce ~ '^[0-9a-f]{64}$')
    ),
    constraint agent_runs_timestamps_check check (
        (finished_at is null or (started_at is not null and finished_at>=started_at))
    ),
    foreign key(job_id,message_id) references dorf.job_messages(job_id,id),
    foreign key(job_id,sandbox_id) references dorf.sandboxes(job_id,id)
);
create unique index agent_runs_one_revision_role on dorf.agent_runs(job_id,input_revision,role)
    where role not in ('implement','investigate');
alter table dorf.agent_runs add constraint agent_runs_job_id_id_key unique(job_id,id);

create table dorf.revisions (
    job_id text not null references dorf.coding_to_proposal_inputs(job_id),
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

create table dorf.evidence (
    id text primary key,
    job_id text not null references dorf.jobs(id),
    digest text not null check (digest ~ '^[0-9a-f]{64}$'),
    byte_size bigint not null check (byte_size>=0),
    media_type text not null check (length(media_type)>0),
    producer text not null check (length(producer)>0),
    kind text not null check (length(kind)>0),
    action_id text references dorf.actions(id),
    agent_run_id text references dorf.agent_runs(id),
    revision text check (revision is null or revision ~ '^[0-9a-f]{40}([0-9a-f]{24})?$'),
    started_at timestamptz not null,
    finished_at timestamptz not null,
    created_at timestamptz not null default clock_timestamp(),
    check (num_nonnulls(action_id,agent_run_id)<=1),
    check (finished_at>=started_at),
    check (kind<>'git-revision' or (agent_run_id is not null and revision is not null))
);
create unique index evidence_one_agent_run on dorf.evidence(agent_run_id)
    where agent_run_id is not null;

create table dorf.artifacts (
    id text primary key,
    job_id text not null references dorf.jobs(id),
    name text not null check (length(trim(name))>0 and length(name)<=255),
    digest text not null check (digest ~ '^[0-9a-f]{64}$'),
    byte_size bigint not null check (byte_size>=0),
    media_type text not null check (length(trim(media_type))>0),
    producer text not null check (length(trim(producer))>0),
    agent_run_id text not null,
    created_at timestamptz not null,
    unique(job_id,name),
    unique(job_id,id),
    unique(job_id,id,agent_run_id),
    foreign key(job_id,agent_run_id) references dorf.agent_runs(job_id,id)
);

create table dorf.codebase_investigation_drafts (
    job_id text not null references dorf.codebase_investigation_sources(job_id),
    agent_run_id text not null,
    artifact_id text not null,
    primary key(job_id,agent_run_id),
    unique(job_id,artifact_id),
    foreign key(job_id,agent_run_id) references dorf.agent_runs(job_id,id),
    foreign key(job_id,artifact_id,agent_run_id) references dorf.artifacts(job_id,id,agent_run_id)
);

alter table dorf.revisions
    add constraint revisions_evidence_fk foreign key(evidence_id) references dorf.evidence(id);

create table dorf.review_plans (
    job_id text not null references dorf.coding_to_proposal_inputs(job_id),
    revision text not null check (revision ~ '^[0-9a-f]{40}([0-9a-f]{24})?$'),
    facts jsonb not null,
    plan jsonb not null,
    policy_digest text not null check (policy_digest ~ '^[0-9a-f]{64}$'),
    created_at timestamptz not null default clock_timestamp(),
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
    coalesce(ar.input_revision,'') as input_revision,
    coalesce(ar.capability,'') as capability,
    ar.started_at,
    ar.finished_at,
    request.from_kind as request_from_kind,
    request.from_id as request_from_id,
    request.sequence as request_sequence,
    request.input as request_input,
    request.delivery_intent as request_delivery_intent,
    coalesce(request.steer_target_turn_id,'') as request_target_turn_id,
    request.admitted_at as request_admitted_at,
    ar.sandbox_id as sandbox_id,
    sandbox.ownership_nonce as ownership_nonce,
    coalesce(ar.submission_nonce,'') as submission_nonce
from dorf.agent_runs ar
join dorf.job_messages request on request.id=ar.message_id and request.job_id=ar.job_id
join dorf.sandboxes sandbox on sandbox.id=ar.sandbox_id;

create table dorf.github_proposals (
    job_id text primary key references dorf.coding_to_proposal_inputs(job_id),
    pr_number bigint not null check (pr_number>0),
    pr_url text not null check (length(pr_url)>0),
    proposed_revision text not null check (proposed_revision ~ '^[0-9a-f]{40}([0-9a-f]{24})?$'),
    body_digest text not null check (body_digest ~ '^[0-9a-f]{64}$')
);

create table dorf.job_outcomes (
    job_id text primary key references dorf.coding_to_proposal_inputs(job_id),
    outcome text not null check (outcome in ('accepted','rejected','abandoned')),
    observed_state text check (observed_state in ('open','closed')),
    observed_merged boolean not null,
    merge_commit_oid text check (merge_commit_oid is null or merge_commit_oid ~ '^[0-9a-f]{40}([0-9a-f]{24})?$'),
    observed_at timestamptz not null default clock_timestamp(),
    check (
        (outcome='accepted' and observed_state is not null and observed_state='closed' and observed_merged and merge_commit_oid is not null) or
        (outcome='rejected' and observed_state is not null and observed_state='closed' and not observed_merged and merge_commit_oid is null) or
        (outcome='abandoned' and not observed_merged and merge_commit_oid is null)
    )
);

comment on schema dorf is 'Dorf-owned product facts; Absurd execution state remains in schema absurd';
comment on table dorf.job_messages is 'Immutable client input and Job-local admission order; follow is FIFO and steer is an explicit priority lane';
comment on table dorf.agent_runs is 'Harness Thread and Turn bindings plus lifecycle outcome; the harness owns transcript and context';
comment on table dorf.evidence is 'Immutable content-addressed Evidence references; bytes live in deployment-owned storage';
comment on table dorf.sandboxes is 'Job-owned isolated workstations used by one or more AgentRuns';
comment on table dorf.sandbox_profiles is 'Named immutable-while-in-use provider, artifact, and Harness definitions selected by Jobs';
comment on table dorf.sandbox_profile_verifications is 'Dorf-owned base-contract proof and confirmed cleanup for one exact Sandbox profile';
comment on table dorf.github_proposals is 'One exact-Revision GitHub proposal projection per Job';
comment on table dorf.job_outcomes is 'Immutable Job outcome; accepted and rejected outcomes retain an exact Proposal observation while pre-publication abandonment has none';
comment on table dorf.codebase_investigation_drafts is 'Immutable investigator drafts; Markdown bytes live in the referenced Artifact';
comment on table dorf.codebase_investigation_sources is 'Immutable remote or retained Git-bundle input for one codebase-investigation Job';

insert into dorf.schema_migrations(name) values ('001_baseline.sql');
