-- Dorf product facts deliberately live outside Absurd's execution schema.
create schema if not exists dorf;

create table if not exists dorf.schema_migrations (
    name text primary key,
    applied_at timestamptz not null default clock_timestamp()
);

create table if not exists dorf.jobs (
    id text primary key,
    admission_key text not null unique,
    goal text not null check (length(trim(goal)) > 0),
    repository text not null check (length(trim(repository)) > 0),
    revision text not null check (length(trim(revision)) > 0),
    branch text not null check (length(trim(branch)) > 0),
    provider_connection text not null check (length(trim(provider_connection)) > 0),
    model text not null check (length(trim(model)) > 0),
    reasoning_effort text not null check (reasoning_effort in ('low', 'medium', 'high', 'xhigh')),
    state text not null default 'admitted' check (state in ('admitted', 'running', 'observed', 'failed')),
    cleanup_state text not null default 'pending' check (cleanup_state in ('pending', 'scheduled', 'complete')),
    task_id text unique,
    cleanup_task_id text unique,
    native_outcome text,
    admitted_at timestamptz not null default clock_timestamp(),
    observed_at timestamptz,
    cleaned_at timestamptz
);

create table if not exists dorf.actions (
    id text primary key,
    job_id text not null references dorf.jobs(id),
    kind text not null check (kind in (
        'sandbox-create', 'repository-clone', 'provider-route-create',
        'codex-session-start', 'codex-turn-start',
        'provider-route-revoke', 'sandbox-delete'
    )),
    state text not null check (state in ('pending', 'succeeded', 'uncertain')),
    external_id text,
    external_outcome text,
    attempts integer not null default 0 check (attempts >= 0),
    created_at timestamptz not null default clock_timestamp(),
    updated_at timestamptz not null default clock_timestamp(),
    unique (job_id, kind)
);

create table if not exists dorf.sandboxes (
    job_id text primary key references dorf.jobs(id),
    action_id text not null unique references dorf.actions(id),
    incus_name text not null unique,
    state text not null check (state in ('created', 'deleted')),
    observed_at timestamptz not null default clock_timestamp()
);

create table if not exists dorf.routes (
    job_id text primary key references dorf.jobs(id),
    action_id text not null unique references dorf.actions(id),
    route_id text not null unique,
    state text not null check (state in ('active', 'revoked')),
    observed_at timestamptz not null default clock_timestamp()
);

create table if not exists dorf.sessions (
    job_id text primary key references dorf.jobs(id),
    action_id text not null unique references dorf.actions(id),
    native_session_id text not null unique,
    observed_at timestamptz not null default clock_timestamp()
);

create table if not exists dorf.agent_runs (
    id text primary key,
    job_id text not null unique references dorf.jobs(id),
    action_id text not null unique references dorf.actions(id),
    session_id text not null references dorf.sessions(native_session_id),
    native_turn_id text not null unique,
    role text not null check (role = 'implement'),
    native_outcome text not null check (native_outcome in ('completed', 'interrupted', 'failed')),
    observed_at timestamptz not null default clock_timestamp()
);

comment on schema dorf is 'Dorf-owned product facts; Absurd execution state remains in schema absurd';
comment on table dorf.agent_runs is 'Bindings and observed native outcomes only; Codex owns transcripts and context';
