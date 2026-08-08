-- The #42 dogfood repair may have left an unregistered prototype table. It is
-- deliberately not a migration source or compatibility surface.
drop table if exists dorf.message_resolutions;

alter table dorf.job_messages
    add column retry_of_message_id text references dorf.job_messages(id),
    add constraint job_messages_job_id_id_key unique(job_id,id);
create unique index job_messages_one_retry
    on dorf.job_messages(retry_of_message_id) where retry_of_message_id is not null;

create view dorf.message_delivery_order as
with recursive delivery_order(job_id,message_id,root_sequence,retry_depth) as (
    select job_id,id,sequence,0
    from dorf.job_messages
    where retry_of_message_id is null
    union all
    select child.job_id,child.id,parent.root_sequence,parent.retry_depth+1
    from delivery_order parent
    join dorf.job_messages child
      on child.job_id=parent.job_id and child.retry_of_message_id=parent.message_id
)
select job_id,message_id,root_sequence,retry_depth
from delivery_order;

create table dorf.message_resolutions (
    id text primary key,
    job_id text not null,
    message_id text not null,
    decision text not null check (decision in ('retry','acknowledge-loss','abandon')),
    authority text not null check (length(trim(authority)) > 0),
    reason text not null check (length(trim(reason)) > 0),
    reserved_wake_sequence bigint check (reserved_wake_sequence > 0),
    retry_message_id text unique references dorf.job_messages(id),
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
return coalesce(run_state = 'completed' or resolution_decision in ('retry','acknowledge-loss','abandon'), false);

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
