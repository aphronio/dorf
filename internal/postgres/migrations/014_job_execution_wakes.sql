create table dorf.job_execution_wakes (
    job_id text primary key references dorf.jobs(id) on delete cascade,
    revision bigint not null default 0 check (revision >= 0)
);

create table dorf.job_execution_wake_causes (
    job_id text not null references dorf.job_execution_wakes(job_id) on delete cascade,
    cause_key text not null check (length(cause_key) between 1 and 512 and cause_key = trim(cause_key)),
    revision bigint not null check (revision > 0),
    primary key(job_id,cause_key),
    unique(job_id,revision)
);

comment on table dorf.job_execution_wakes is 'Disposable per-Job revision used only to select fresh Absurd wake events';
comment on table dorf.job_execution_wake_causes is 'Bounded idempotency keys for Message, Stop, and exact native Turn wake hints';

insert into dorf.schema_migrations(name) values ('014_job_execution_wakes.sql');
