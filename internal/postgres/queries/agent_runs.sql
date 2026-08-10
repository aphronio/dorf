-- name: InsertInitialAgentRun :exec
insert into dorf.agent_runs(id,job_id,message_id,action_id,role,state)
values(sqlc.arg(id),sqlc.arg(job_id),sqlc.arg(message_id),sqlc.arg(action_id),'implement','pending')
on conflict do nothing;

-- name: InsertAgentRun :exec
insert into dorf.agent_runs(id,job_id,message_id,action_id,session_id,role,state)
values(
    sqlc.arg(id),sqlc.arg(job_id),sqlc.arg(message_id),sqlc.arg(action_id),
    nullif(sqlc.arg(session_id)::text,''),sqlc.arg(role),'pending'
);

-- name: InsertAgentRunIfAbsent :exec
insert into dorf.agent_runs(id,job_id,message_id,action_id,session_id,role,state)
values(
    sqlc.arg(id),sqlc.arg(job_id),sqlc.arg(message_id),sqlc.arg(action_id),
    nullif(sqlc.arg(session_id)::text,''),'implement','pending'
)
on conflict do nothing;

-- name: InsertAgentRunFromImplementationSession :execrows
insert into dorf.agent_runs(id,job_id,message_id,action_id,session_id,role,state)
select sqlc.arg(id),sqlc.arg(job_id),sqlc.arg(message_id),sqlc.arg(action_id),
       native_session_id,'implement','pending'
from dorf.sessions
where job_id=sqlc.arg(job_id);

-- name: BindAgentRunSessionByMessage :exec
update dorf.agent_runs
set session_id=coalesce(session_id,sqlc.arg(session_id)),updated_at=clock_timestamp()
where message_id=sqlc.arg(message_id)
  and (session_id is null or session_id=sqlc.arg(session_id));

-- name: BindReviewAgentRunSession :execrows
update dorf.agent_runs
set session_id=coalesce(session_id,sqlc.arg(session_id)),updated_at=clock_timestamp()
where id=sqlc.arg(run_id) and (session_id is null or session_id=sqlc.arg(session_id));

-- name: BindImplementationAgentRunSessions :exec
update dorf.agent_runs
set session_id=sqlc.arg(session_id),updated_at=clock_timestamp()
where job_id=sqlc.arg(job_id) and role='implement'
  and (session_id is null or session_id=sqlc.arg(session_id));

-- name: GetAgentRunByMessage :one
select id,job_id,coalesce(message_id,'') as message_id,action_id,
       coalesce(session_id,'') as session_id,state,
       (baseline_native_turn_id is not null)::boolean as baseline_recorded,
       coalesce(baseline_native_turn_id,'') as baseline_native_turn_id,
       coalesce(native_turn_id,'') as native_turn_id,
       coalesce(native_outcome,'') as native_outcome,
       coalesce(attention,'') as attention,role
from dorf.agent_runs
where message_id=sqlc.arg(message_id)::text;

-- name: BlockAgentRunDelivery :exec
update dorf.agent_runs
set state='uncertain',attention=sqlc.arg(reason),updated_at=clock_timestamp()
where id=sqlc.arg(run_id) and state not in ('completed','failed','interrupted');

-- name: PrepareAgentRun :execrows
update dorf.agent_runs
set state='submitting',baseline_native_turn_id=sqlc.arg(baseline_turn_id),
    attention=null,started_at=coalesce(started_at,clock_timestamp()),
    updated_at=clock_timestamp()
where id=sqlc.arg(run_id) and state='pending';

-- name: GetAgentRunBaseline :one
select (baseline_native_turn_id is not null)::boolean as recorded,
       coalesce(baseline_native_turn_id,'') as baseline_turn_id
from dorf.agent_runs
where id=sqlc.arg(run_id);

-- name: BindNativeTurn :one
update dorf.agent_runs
set native_turn_id=coalesce(native_turn_id,sqlc.arg(turn_id)),
    state=sqlc.arg(state),native_outcome=nullif(sqlc.arg(outcome)::text,''),
    attention=nullif(sqlc.arg(attention)::text,''),
    finished_at=case when sqlc.arg(state)::text in ('completed','failed','interrupted')
        then coalesce(finished_at,clock_timestamp()) else finished_at end,
    updated_at=clock_timestamp()
where id=sqlc.arg(run_id) and (native_turn_id is null or native_turn_id=sqlc.arg(turn_id))
returning action_id;

-- name: PropagateNativeTurnOutcomeToSteers :exec
update dorf.agent_runs accepted
set native_outcome=sqlc.arg(outcome),updated_at=clock_timestamp()
from dorf.job_messages message,dorf.agent_runs source
where source.id=sqlc.arg(run_id) and accepted.id<>source.id
  and accepted.job_id=source.job_id and accepted.session_id=source.session_id
  and accepted.message_id=message.id and message.delivery_intent='steer'
  and message.steer_target_turn_id=sqlc.arg(turn_id)
  and accepted.native_turn_id=sqlc.arg(turn_id)
  and accepted.state='completed' and accepted.native_outcome is null;

-- name: BindNativeSteer :one
update dorf.agent_runs
set native_turn_id=coalesce(native_turn_id,sqlc.arg(turn_id)),state='completed',
    native_outcome=coalesce(native_outcome,nullif(sqlc.arg(outcome)::text,'')),
    attention=null,finished_at=coalesce(finished_at,clock_timestamp()),
    updated_at=clock_timestamp()
where id=sqlc.arg(run_id) and (native_turn_id is null or native_turn_id=sqlc.arg(turn_id))
returning action_id,coalesce(native_outcome,'') as native_outcome;

-- name: FailAgentRun :one
update dorf.agent_runs
set state='failed',
    native_outcome=case when native_turn_id is null then null else 'failed' end,
    attention=sqlc.arg(reason),updated_at=clock_timestamp()
where id=sqlc.arg(run_id)
returning action_id;

-- name: MarkAgentRunUncertain :one
update dorf.agent_runs
set state='uncertain',attention=sqlc.arg(reason),updated_at=clock_timestamp()
where id=sqlc.arg(run_id)
returning action_id;

-- name: SetAgentRunAttention :execrows
update dorf.agent_runs
set attention=sqlc.arg(reason),updated_at=clock_timestamp()
where id=sqlc.arg(run_id);

-- name: GetNativeMutationDelivery :one
select m.id as message_id,m.job_id,m.from_kind,m.from_id,m.sequence,m.input,
       m.delivery_intent,coalesce(m.steer_target_turn_id,'') as steer_target_turn_id,
       ar.id as agent_run_id,ar.job_id as agent_run_job_id,
       coalesce(ar.message_id,'') as agent_run_message_id,ar.action_id,
       coalesce(ar.session_id,'') as session_id,ar.state,
       (ar.baseline_native_turn_id is not null)::boolean as baseline_recorded,
       coalesce(ar.baseline_native_turn_id,'') as baseline_native_turn_id,
       coalesce(ar.native_turn_id,'') as native_turn_id,
       coalesce(ar.native_outcome,'') as native_outcome,
       coalesce(ar.attention,'') as attention,ar.role
from dorf.job_messages m
join dorf.agent_runs ar on ar.message_id=m.id
where m.job_id=sqlc.arg(job_id) and ar.state in ('submitting','active','uncertain')
order by m.sequence
limit 1;

-- name: CountImplementationNativeMutations :one
select count(*)
from dorf.agent_runs
where job_id=sqlc.arg(job_id) and role='implement'
  and state in ('submitting','active','uncertain');
