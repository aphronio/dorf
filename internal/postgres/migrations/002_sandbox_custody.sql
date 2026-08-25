-- Freeze the v0.3.x baseline and advance retained PostgreSQL deployments to
-- Sandbox-handle custody. Some development deployments were initialized from
-- a later edited copy of 001_baseline.sql, so this migration also converges
-- that exact already-current shape.

alter table dorf.jobs drop constraint if exists jobs_cleanup_state_check;
alter table dorf.jobs add constraint jobs_cleanup_state_check
    check (cleanup_state in ('pending','requested','scheduled','complete'));

alter table dorf.sandboxes add column if not exists name text;

update dorf.sandboxes sandbox
set name='default'
where sandbox.name is null
  and exists (
    select 1 from dorf.agent_runs run
    where run.sandbox_id=sandbox.id and run.job_id=sandbox.job_id
      and run.role in ('implement','investigate')
  );

do $$
begin
    if exists (
        select sandbox.id
        from dorf.sandboxes sandbox
        left join dorf.agent_runs run
          on run.sandbox_id=sandbox.id and run.job_id=sandbox.job_id
        where sandbox.name is null
        group by sandbox.id
        having count(run.id)<>1
    ) then
        raise exception 'cannot migrate non-primary Sandbox without exactly one owning AgentRun';
    end if;
end
$$;

update dorf.sandboxes sandbox
set name=(
    select min(run.id) from dorf.agent_runs run
    where run.sandbox_id=sandbox.id and run.job_id=sandbox.job_id
)
where sandbox.name is null;

do $$
begin
    if exists (select 1 from dorf.sandboxes where name is null) then
        raise exception 'cannot migrate Sandbox without an owning AgentRun';
    end if;
end
$$;

alter table dorf.sandboxes alter column name set not null;
alter table dorf.sandboxes drop constraint if exists sandboxes_name_check;
alter table dorf.sandboxes add constraint sandboxes_name_check
    check (name ~ '^[a-z][a-z0-9-]{0,126}$');
alter table dorf.sandboxes drop constraint if exists sandboxes_job_id_name_key;
alter table dorf.sandboxes add constraint sandboxes_job_id_name_key unique(job_id,name);

drop table if exists dorf.codebase_investigation_drafts;
drop table if exists dorf.artifacts;

drop view dorf.review_run_projection;
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
    sandbox.name as sandbox_name,
    sandbox.ownership_nonce as ownership_nonce,
    coalesce(ar.submission_nonce,'') as submission_nonce
from dorf.agent_runs ar
join dorf.job_messages request on request.id=ar.message_id and request.job_id=ar.job_id
join dorf.sandboxes sandbox on sandbox.id=ar.sandbox_id;

insert into dorf.schema_migrations(name) values ('002_sandbox_custody.sql');
