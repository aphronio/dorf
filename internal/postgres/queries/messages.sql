-- name: InsertInitialMessage :exec
insert into dorf.job_messages(id,job_id,from_kind,from_id,sequence,input)
values(sqlc.arg(id),sqlc.arg(job_id),'human',sqlc.arg(from_id),1,sqlc.arg(input))
on conflict(job_id,from_kind,from_id) do nothing;

-- name: GetInitialMessage :one
select id,sequence,input
from dorf.job_messages
where job_id=sqlc.arg(job_id) and from_kind='human' and from_id=sqlc.arg(from_id);

-- name: GetMessageBySender :one
select id,job_id,from_kind,from_id,sequence,input,delivery_intent,
       coalesce(steer_target_turn_id,'') as steer_target_turn_id
from dorf.job_messages
where job_id=sqlc.arg(job_id) and from_kind=sqlc.arg(from_kind)
  and from_id=sqlc.arg(from_id);

-- name: GetActiveImplementationTurn :one
select coalesce(native_turn_id,'') as native_turn_id,coalesce(session_id,'') as session_id,role
from dorf.agent_runs
where job_id=sqlc.arg(job_id) and state='active' and native_turn_id is not null
  and role='implement'
order by started_at,id
limit 1;

-- name: NextMessageSequence :one
select (coalesce(max(sequence),0)+1)::bigint
from dorf.job_messages
where job_id=sqlc.arg(job_id);

-- name: InsertMessage :exec
insert into dorf.job_messages(
    id,job_id,from_kind,from_id,sequence,input,delivery_intent,steer_target_turn_id
)
values(
    sqlc.arg(id),sqlc.arg(job_id),sqlc.arg(from_kind),sqlc.arg(from_id),
    sqlc.arg(sequence),sqlc.arg(input),sqlc.arg(delivery_intent),
    nullif(sqlc.arg(steer_target_turn_id)::text,'')
);

-- name: ReopenPublishedForFollow :execrows
update dorf.jobs
set workflow_phase='implementing',workflow_attention=null
where id=sqlc.arg(job_id) and workflow_phase='published'
  and not exists (select 1 from dorf.job_outcomes where job_id=dorf.jobs.id);

-- name: GetFirstUnsettledInput :one
select m.sequence,coalesce(ar.state,'') as state,coalesce(ar.attention,'') as attention
from dorf.job_messages m
left join dorf.agent_runs ar on ar.message_id=m.id
where m.job_id=sqlc.arg(job_id)
  and (ar.native_turn_id is null or ar.state not in ('completed','failed','interrupted'))
order by m.sequence
limit 1;

-- name: CountUnsettledInputs :one
select count(*)
from dorf.job_messages m
left join dorf.agent_runs ar on ar.message_id=m.id
where m.job_id=sqlc.arg(job_id)
  and (ar.native_turn_id is null or ar.state not in ('completed','failed','interrupted'));

-- name: GetLatestFollowRun :one
select ar.id,ar.job_id,ar.state,ar.role
from dorf.job_messages m
join dorf.agent_runs ar on ar.message_id=m.id
where m.job_id=sqlc.arg(job_id) and m.delivery_intent='follow'
order by m.sequence desc
limit 1;

-- name: GetCheckMessage :one
select id,job_id,from_kind,from_id,sequence,input,delivery_intent,
       coalesce(steer_target_turn_id,'') as steer_target_turn_id
from dorf.job_messages
where job_id=sqlc.arg(job_id) and from_kind='workflow' and from_id=sqlc.arg(from_id);

-- name: ListMessages :many
select m.id,m.job_id,m.from_kind,m.from_id,m.sequence,m.input,m.delivery_intent,
       coalesce(m.steer_target_turn_id,'') as steer_target_turn_id,
       coalesce(ar.id,'') as agent_run_id,coalesce(ar.state,'') as state,
       coalesce(ar.native_turn_id,'') as native_turn_id,
       coalesce(ar.native_outcome,'') as native_outcome,
       coalesce(ar.attention,'') as attention,
       (ar.native_turn_id is not null)::boolean as delivered
from dorf.job_messages m
left join dorf.agent_runs ar on ar.message_id=m.id
where m.job_id=sqlc.arg(job_id)
order by m.sequence;

-- name: NextWakeSequence :one
select coalesce(
    (
        select m.sequence
        from dorf.jobs j
        join dorf.actions a on a.id=j.setup_action_id
        join dorf.job_messages m
          on m.job_id=j.id and m.from_kind='workflow' and m.from_id=a.scope_key
        where j.id=sqlc.arg(job_id) and j.workflow_phase='setup'
          and a.kind='repository-setup' and a.scope_key<>''
          and a.state in ('pending','uncertain')
    ),
    (
        select min(m.sequence)
        from dorf.job_messages m
        join dorf.agent_runs ar on ar.message_id=m.id
        where m.job_id=sqlc.arg(job_id) and m.sequence>1 and ar.native_turn_id is null
          and not exists (
              select 1
              from dorf.job_messages earlier
              join dorf.agent_runs earlier_run on earlier_run.message_id=earlier.id
              where earlier.job_id=m.job_id and earlier.sequence<m.sequence
                and (earlier_run.native_turn_id is null
                     or earlier_run.state not in ('completed','failed','interrupted'))
          )
    ),
    (select coalesce(max(sequence),0)+1 from dorf.job_messages where job_id=sqlc.arg(job_id))
)::bigint;
