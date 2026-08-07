alter table dorf.jobs
    add column if not exists starting_revision text,
    add column if not exists workflow_phase text not null default 'setup',
    add column if not exists repair_count integer not null default 0,
    add column if not exists workflow_attention text;

update dorf.jobs set starting_revision=revision where starting_revision is null;
alter table dorf.jobs alter column starting_revision set not null;

do $$
begin
    if not exists (
        select 1 from pg_constraint
        where conrelid='dorf.jobs'::regclass and conname='jobs_workflow_phase_check'
    ) then
        alter table dorf.jobs add constraint jobs_workflow_phase_check check (
            workflow_phase in ('setup','implementing','checking','repairing','ready','blocked')
        );
    end if;
    if not exists (
        select 1 from pg_constraint
        where conrelid='dorf.jobs'::regclass and conname='jobs_repair_count_check'
    ) then
        alter table dorf.jobs add constraint jobs_repair_count_check check (repair_count between 0 and 1);
    end if;
end
$$;

alter table dorf.actions drop constraint if exists actions_kind_check;
alter table dorf.actions add constraint actions_kind_check check (kind in (
    'sandbox-create','repository-clone','repository-setup','repository-commit',
    'provider-route-create','codex-session-start','codex-turn-start',
    'provider-route-revoke','sandbox-delete'
));
alter table dorf.actions add column if not exists scope_key text not null default '';
drop index if exists dorf.actions_one_job_effect;
create unique index if not exists actions_one_unscoped_job_effect
    on dorf.actions(job_id,kind) where message_id is null and scope_key='';
create unique index if not exists actions_one_scoped_job_effect
    on dorf.actions(job_id,kind,scope_key) where message_id is null and scope_key<>'';

alter table dorf.agent_runs drop constraint if exists agent_runs_role_check;
alter table dorf.agent_runs add constraint agent_runs_role_check check (role in ('implement','repair'));

create table if not exists dorf.revisions (
    job_id text not null references dorf.jobs(id),
    oid text not null check (oid ~ '^[0-9a-f]{40}([0-9a-f]{24})?$'),
    parent_oid text check (parent_oid is null or parent_oid ~ '^[0-9a-f]{40}([0-9a-f]{24})?$'),
    tree_oid text check (tree_oid is null or tree_oid ~ '^[0-9a-f]{40}([0-9a-f]{24})?$'),
    branch text not null,
    generation integer not null check (generation >= 0),
    action_id text references dorf.actions(id),
    observed_at timestamptz not null default clock_timestamp(),
    primary key(job_id,oid),
    unique(job_id,generation)
);

create table if not exists dorf.repository_commands (
    job_id text not null references dorf.jobs(id),
    name text not null check (name in ('prepare','check','smoke')),
    command text not null check (length(command) > 0),
    starting_revision text not null check (starting_revision ~ '^[0-9a-f]{40}([0-9a-f]{24})?$'),
    primary key(job_id,name)
);

comment on table dorf.repository_commands is 'Small direct repository contract pinned from the exact starting Revision';

insert into dorf.revisions(job_id,oid,branch,generation)
select id,starting_revision,branch,0 from dorf.jobs
on conflict do nothing;

create table if not exists dorf.checks (
    id text primary key,
    job_id text not null references dorf.jobs(id),
    name text not null check (name in ('check','smoke')),
    command text not null check (length(command) > 0),
    revision text not null check (revision ~ '^[0-9a-f]{40}([0-9a-f]{24})?$'),
    state text not null check (state in ('running','passed','failed')),
    exit_code integer,
    evidence_id text,
    started_at timestamptz,
    finished_at timestamptz,
    attempts integer not null default 0 check (attempts >= 0),
    attention text,
    unique(job_id,revision,name)
);

create table if not exists dorf.evidence (
    id text primary key,
    job_id text not null references dorf.jobs(id),
    digest text not null check (digest ~ '^[0-9a-f]{64}$'),
    byte_size bigint not null check (byte_size >= 0),
    media_type text not null check (length(media_type) > 0),
    producer text not null check (length(producer) > 0),
    provenance text not null check (provenance in ('observed','claim')),
    kind text not null check (length(kind) > 0),
    action_id text references dorf.actions(id),
    check_id text references dorf.checks(id),
    revision text check (revision is null or revision ~ '^[0-9a-f]{40}([0-9a-f]{24})?$'),
    started_at timestamptz not null,
    finished_at timestamptz not null,
    created_at timestamptz not null default clock_timestamp(),
    check (check_id is null or action_id is null),
    check (finished_at >= started_at)
);

comment on table dorf.evidence is 'Immutable content-addressed Evidence references; bytes live in the deployment-owned local store';
comment on table dorf.checks is 'Deterministic command observations tied to one exact Git Revision';

alter table dorf.checks add constraint checks_evidence_id_fkey
    foreign key(evidence_id) references dorf.evidence(id);
