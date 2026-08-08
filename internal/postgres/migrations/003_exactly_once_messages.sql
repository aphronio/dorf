alter table dorf.jobs
    add column if not exists admission_open boolean not null default true,
    add column if not exists native_outcome text,
    add column if not exists observed_at timestamptz;

create table if not exists dorf.job_messages (
    id text primary key,
    job_id text not null references dorf.jobs(id),
    caller_id text not null check (length(trim(caller_id)) > 0),
    sequence bigint not null check (sequence > 0),
    input text not null check (length(trim(input)) > 0),
    delivery_intent text not null default 'follow' check (delivery_intent in ('follow','steer')),
    steer_target_turn_id text,
    admitted_at timestamptz not null default clock_timestamp(),
    check ((delivery_intent='follow' and steer_target_turn_id is null) or
           (delivery_intent='steer' and steer_target_turn_id is not null)),
    unique (job_id, caller_id),
    unique (job_id, sequence)
);

-- Preserve already-durable issue #40 Jobs. Their initial input becomes FIFO
-- sequence 1, while their existing native Action and AgentRun identities stay
-- intact. New admissions use the hashed application identity; this legacy
-- identity is deterministic and is never regenerated as a new logical input.
insert into dorf.job_messages(id,job_id,caller_id,sequence,input,admitted_at)
select 'message-legacy-' || j.id,j.id,'dorf:initial',1,j.goal,j.admitted_at
from dorf.jobs j
on conflict(job_id,caller_id) do nothing;

alter table dorf.actions drop constraint if exists actions_job_id_kind_key;
alter table dorf.actions add column if not exists message_id text references dorf.job_messages(id);
update dorf.actions a
set message_id=m.id
from dorf.job_messages m
where a.job_id=m.job_id and a.kind='codex-turn-start' and a.message_id is null and m.sequence=1;
alter table dorf.actions drop constraint if exists actions_state_check;
alter table dorf.actions add constraint actions_state_check
    check (state in ('pending', 'succeeded', 'failed', 'uncertain'));
create unique index if not exists actions_one_job_effect
    on dorf.actions(job_id, kind) where message_id is null;
create unique index if not exists actions_one_message_effect
    on dorf.actions(message_id, kind) where message_id is not null;

alter table dorf.agent_runs drop constraint if exists agent_runs_job_id_key;
alter table dorf.agent_runs drop constraint if exists agent_runs_native_turn_id_key;
alter table dorf.agent_runs alter column native_turn_id drop not null;
alter table dorf.agent_runs alter column native_outcome drop not null;
alter table dorf.agent_runs add column if not exists message_id text references dorf.job_messages(id);
alter table dorf.agent_runs add column if not exists state text not null default 'pending';
alter table dorf.agent_runs add column if not exists baseline_native_turn_id text;
alter table dorf.agent_runs add column if not exists attention text;
alter table dorf.agent_runs add column if not exists updated_at timestamptz not null default clock_timestamp();
alter table dorf.agent_runs add column if not exists observed_at timestamptz not null default clock_timestamp();
update dorf.agent_runs ar
set message_id=m.id,
    state=case ar.native_outcome
        when 'completed' then 'completed'
        when 'failed' then 'failed'
        when 'interrupted' then 'interrupted'
        else 'uncertain'
    end,
    baseline_native_turn_id='',
    attention=case when ar.native_outcome in ('completed','failed','interrupted') then null
        else 'legacy native outcome is unsupported and requires inspection'
    end,
    updated_at=ar.observed_at
from dorf.job_messages m
where ar.job_id=m.job_id and ar.message_id is null and m.sequence=1;
alter table dorf.agent_runs alter column message_id set not null;
alter table dorf.agent_runs alter column session_id drop not null;
alter table dorf.agent_runs drop constraint if exists agent_runs_state_check;
alter table dorf.agent_runs add constraint agent_runs_state_check
    check (state in ('pending', 'submitting', 'active', 'completed', 'failed', 'interrupted', 'uncertain'));
drop index if exists dorf.agent_runs_one_message;
create unique index agent_runs_one_message on dorf.agent_runs(message_id);

alter table dorf.actions drop constraint if exists actions_turn_message_check;
alter table dorf.actions add constraint actions_turn_message_check
    check ((kind = 'codex-turn-start') = (message_id is not null));

comment on table dorf.job_messages is 'Immutable Dorf-owned client input, explicit harness delivery intent, and per-Job FIFO position';
comment on table dorf.agent_runs is 'Per-input harness acceptance binding and native outcome evidence only; Codex owns transcript and context';
