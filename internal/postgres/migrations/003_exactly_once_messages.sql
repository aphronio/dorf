alter table dorf.jobs
    add column if not exists admission_open boolean not null default true;
alter table dorf.jobs
    drop column if exists native_outcome,
    drop column if exists observed_at;

create table if not exists dorf.job_messages (
    id text primary key,
    job_id text not null references dorf.jobs(id),
    caller_id text not null check (length(trim(caller_id)) > 0),
    sequence bigint not null check (sequence > 0),
    input text not null check (length(trim(input)) > 0),
    admitted_at timestamptz not null default clock_timestamp(),
    unique (job_id, caller_id),
    unique (job_id, sequence)
);

alter table dorf.actions drop constraint if exists actions_job_id_kind_key;
alter table dorf.actions add column if not exists message_id text references dorf.job_messages(id);
alter table dorf.actions drop constraint if exists actions_state_check;
alter table dorf.actions add constraint actions_state_check
    check (state in ('pending', 'succeeded', 'failed', 'uncertain'));
create unique index if not exists actions_one_job_effect
    on dorf.actions(job_id, kind) where message_id is null;
create unique index if not exists actions_one_message_effect
    on dorf.actions(message_id, kind) where message_id is not null;

alter table dorf.agent_runs drop constraint if exists agent_runs_job_id_key;
alter table dorf.agent_runs alter column native_turn_id drop not null;
alter table dorf.agent_runs alter column native_outcome drop not null;
alter table dorf.agent_runs add column if not exists message_id text references dorf.job_messages(id);
alter table dorf.agent_runs add column if not exists state text not null default 'pending';
alter table dorf.agent_runs add column if not exists baseline_native_turn_id text;
alter table dorf.agent_runs add column if not exists attention text;
alter table dorf.agent_runs add column if not exists updated_at timestamptz not null default clock_timestamp();
delete from dorf.agent_runs where message_id is null;
delete from dorf.actions where kind = 'codex-turn-start' and message_id is null;
alter table dorf.agent_runs alter column message_id set not null;
alter table dorf.agent_runs alter column session_id drop not null;
alter table dorf.agent_runs drop column if exists observed_at;
alter table dorf.agent_runs drop constraint if exists agent_runs_state_check;
alter table dorf.agent_runs add constraint agent_runs_state_check
    check (state in ('pending', 'submitting', 'active', 'completed', 'failed', 'interrupted', 'uncertain'));
drop index if exists dorf.agent_runs_one_message;
create unique index agent_runs_one_message on dorf.agent_runs(message_id);

alter table dorf.actions drop constraint if exists actions_turn_message_check;
alter table dorf.actions add constraint actions_turn_message_check
    check ((kind = 'codex-turn-start') = (message_id is not null));

comment on table dorf.job_messages is 'Immutable Dorf-owned client input and per-Job FIFO position';
comment on table dorf.agent_runs is 'Per-input native binding and delivery truth only; Codex owns transcript and context';
