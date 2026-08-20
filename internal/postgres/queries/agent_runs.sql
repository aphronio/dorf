-- name: InsertImplementationAgentRun :execrows
insert into dorf.agent_runs(id,job_id,message_id,harness,thread_id,role,state,sandbox_id)
select sqlc.arg(id),j.id,sqlc.arg(message_id),prior.harness,prior.thread_id,'implement','pending',sqlc.arg(sandbox_id)
from dorf.jobs j
left join lateral (
    select ar.harness,ar.thread_id
    from dorf.agent_runs ar
    left join dorf.job_messages m on m.id=ar.message_id
    where ar.job_id=j.id and ar.role='implement' and ar.thread_id is not null
    order by m.sequence desc nulls last,ar.started_at desc nulls last,ar.id desc
    limit 1
) prior on true
where j.id=sqlc.arg(job_id)
on conflict do nothing;

-- name: InsertInvestigationAgentRun :execrows
insert into dorf.agent_runs(
    id,job_id,message_id,role,state,input_revision,capability,sandbox_id
)
select sqlc.arg(id),j.id,sqlc.arg(message_id),'investigate','pending',
       sqlc.arg(input_revision),'repository-read-report',sqlc.arg(sandbox_id)
from dorf.jobs j
where j.id=sqlc.arg(job_id)
  and j.workflow_name='codebase-investigation'
  and j.workflow_revision='2'
on conflict do nothing;

-- name: ListImplementationThreadBindings :many
select harness,thread_id
from dorf.agent_runs
where job_id=sqlc.arg(job_id) and role='implement' and thread_id is not null
order by id;

-- name: ImplementationThreadExists :one
select exists(
    select 1 from dorf.agent_runs
    where role='implement' and harness=sqlc.arg(harness) and thread_id=sqlc.arg(thread_id)
);

-- name: GetAgentRunByMessage :one
select id,job_id,message_id,state,
       coalesce(harness,'') as harness,coalesce(thread_id,'') as thread_id,
       (baseline_turn_id is not null)::boolean as baseline_recorded,
       coalesce(baseline_turn_id,'') as baseline_turn_id,
       coalesce(turn_id,'') as turn_id,coalesce(turn_outcome,'') as turn_outcome,
       coalesce(attention,'') as attention,role,coalesce(input_revision,'') as input_revision,
       coalesce(sandbox_id,'') as sandbox_id
from dorf.agent_runs
where message_id=sqlc.arg(message_id)::text;

-- name: BindImplementationInputRevision :execrows
update dorf.agent_runs
set input_revision=coalesce(input_revision,sqlc.arg(input_revision))
where id=sqlc.arg(run_id) and role='implement' and state='pending'
  and (input_revision is null or input_revision=sqlc.arg(input_revision));

-- name: GetAgentRunForBinding :one
select job_id,role,state,coalesce(harness,'') as harness,
       coalesce(thread_id,'') as thread_id,coalesce(turn_id,'') as turn_id,
       coalesce(turn_outcome,'') as turn_outcome
from dorf.agent_runs
where id=sqlc.arg(run_id)
for update;

-- name: PrepareAgentRun :execrows
update dorf.agent_runs
set state='submitting',harness=coalesce(harness,sqlc.arg(harness)),
    baseline_turn_id=sqlc.arg(baseline_turn_id),
    attention=null,started_at=coalesce(started_at,clock_timestamp())
where id=sqlc.arg(run_id) and state='pending'
  and (harness is null or harness=sqlc.arg(harness));

-- name: GetAgentRunPreparation :one
select coalesce(harness,'') as harness,(baseline_turn_id is not null)::boolean as recorded,
       coalesce(baseline_turn_id,'') as baseline_turn_id
from dorf.agent_runs
where id=sqlc.arg(run_id);

-- name: BindAgentRunIdentity :execrows
update dorf.agent_runs
set harness=coalesce(harness,sqlc.arg(harness)),
    thread_id=coalesce(thread_id,sqlc.arg(thread_id))
where id=sqlc.arg(run_id)
  and (harness is null or harness=sqlc.arg(harness))
  and (thread_id is null or thread_id=sqlc.arg(thread_id));

-- name: BindHarnessTurn :execrows
update dorf.agent_runs
set turn_id=coalesce(turn_id,sqlc.arg(turn_id)),state=sqlc.arg(state),
    turn_outcome=nullif(sqlc.arg(turn_outcome)::text,''),attention=nullif(sqlc.arg(attention)::text,''),
    finished_at=case when sqlc.arg(state)::text in ('completed','failed','interrupted')
        then coalesce(finished_at,clock_timestamp()) else finished_at end
where id=sqlc.arg(run_id) and harness=sqlc.arg(harness) and thread_id=sqlc.arg(thread_id)
  and (turn_id is null or turn_id=sqlc.arg(turn_id));

-- name: PropagateTurnOutcomeToSteers :exec
update dorf.agent_runs accepted
set turn_outcome=sqlc.arg(turn_outcome)
from dorf.job_messages message,dorf.agent_runs source
where source.id=sqlc.arg(run_id) and accepted.id<>source.id
  and accepted.job_id=source.job_id and accepted.harness=source.harness and accepted.thread_id=source.thread_id
  and accepted.message_id=message.id and message.delivery_intent='steer'
  and message.steer_target_turn_id=sqlc.arg(turn_id)
  and accepted.turn_id=sqlc.arg(turn_id)
  and accepted.state='completed' and accepted.turn_outcome is null;

-- name: BindSteer :one
update dorf.agent_runs
set turn_id=coalesce(turn_id,sqlc.arg(turn_id)),state='completed',
    turn_outcome=coalesce(turn_outcome,nullif(sqlc.arg(turn_outcome)::text,'')),
    attention=null,finished_at=coalesce(finished_at,clock_timestamp())
where id=sqlc.arg(run_id) and harness is not null and thread_id is not null
  and state not in ('failed','interrupted')
  and (turn_id is null or turn_id=sqlc.arg(turn_id))
returning coalesce(turn_outcome,'') as turn_outcome;

-- name: FailAgentRun :execrows
update dorf.agent_runs
set state='failed',
    turn_outcome=case when turn_id is null then null else 'failed' end,
    attention=sqlc.arg(reason),
    finished_at=case when started_at is null then null else coalesce(finished_at,clock_timestamp()) end
where id=sqlc.arg(run_id) and state not in ('completed','interrupted');

-- name: InterruptAgentRun :execrows
update dorf.agent_runs
set state='interrupted',
    turn_outcome=case when turn_id is null then null else 'interrupted' end,
    attention=sqlc.arg(reason)::text,
    finished_at=case when started_at is null then null else coalesce(finished_at,clock_timestamp()) end
where id=sqlc.arg(run_id) and state in ('pending','submitting','active','uncertain');

-- name: MarkAgentRunUncertain :execrows
update dorf.agent_runs
set state='uncertain',attention=sqlc.arg(reason)
where id=sqlc.arg(run_id) and state not in ('completed','failed','interrupted');

-- name: SetAgentRunAttention :execrows
update dorf.agent_runs
set attention=sqlc.arg(reason)
where id=sqlc.arg(run_id);

-- name: GetHarnessMutationDelivery :one
select m.id as message_id,m.job_id,m.from_kind,m.from_id,m.sequence,m.input,m.admitted_at,
       m.delivery_intent,coalesce(m.steer_target_turn_id,'') as steer_target_turn_id,
       ar.id as agent_run_id,ar.job_id as agent_run_job_id,
       ar.message_id as agent_run_message_id,ar.state,
       coalesce(ar.harness,'') as harness,coalesce(ar.thread_id,'') as thread_id,
       (ar.baseline_turn_id is not null)::boolean as baseline_recorded,
       coalesce(ar.baseline_turn_id,'') as baseline_turn_id,
       coalesce(ar.turn_id,'') as turn_id,coalesce(ar.turn_outcome,'') as turn_outcome,
       coalesce(ar.attention,'') as attention,ar.role,
       coalesce(ar.input_revision,'') as input_revision
from dorf.job_messages m
join dorf.agent_runs ar on ar.message_id=m.id
where m.job_id=sqlc.arg(job_id) and ar.state in ('submitting','active','uncertain')
  and ar.role in ('implement','investigate')
order by m.sequence
limit 1;
