-- The #42 dogfood repair may have left an unregistered prototype table. It is
-- deliberately not a migration source or compatibility surface.
drop table if exists dorf.message_resolutions;

alter table dorf.job_messages
    add constraint job_messages_job_id_id_key unique(job_id,id);

create table dorf.message_resolutions (
    id text primary key,
    job_id text not null,
    message_id text not null,
    decision text not null check (decision in ('retry','acknowledge-loss','abandon')),
    authority text not null check (length(trim(authority)) > 0),
    reason text not null check (length(trim(reason)) > 0),
    reserved_wake_sequence bigint check (reserved_wake_sequence > 0),
    resolved_at timestamptz not null default clock_timestamp(),
    unique (job_id,message_id),
    foreign key (job_id,message_id) references dorf.job_messages(job_id,id),
    unique (job_id,reserved_wake_sequence)
);

comment on table dorf.message_resolutions is
    'Append-only operator authority for one exact admitted input; original Message, Action, AgentRun, and native outcome remain evidence';

create function dorf.message_is_settled(run_state text, resolution_decision text)
returns boolean
language sql
immutable
return coalesce(run_state = 'completed' or resolution_decision in ('acknowledge-loss','abandon'), false);

comment on function dorf.message_is_settled(text,text) is
    'Canonical settled-input predicate for FIFO delivery and every workflow gate';

create function dorf.reject_message_resolution_mutation()
returns trigger
language plpgsql
as $$
begin
    raise exception 'message resolution receipts are append-only';
end
$$;

create trigger message_resolutions_append_only
before update or delete on dorf.message_resolutions
for each row execute function dorf.reject_message_resolution_mutation();
