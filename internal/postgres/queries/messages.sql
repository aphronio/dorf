-- name: GetMessageBySender :one
select id,job_id,from_kind,from_id,sequence,input,delivery_intent,requested_intent,
       coalesce(steer_target_turn_id,'') as steer_target_turn_id,admitted_at,refresh_skills
from dorf.job_messages
where job_id=sqlc.arg(job_id) and from_kind=sqlc.arg(from_kind)
  and from_id=sqlc.arg(from_id);

-- name: GetMessage :one
select id,job_id,from_kind,from_id,sequence,input,delivery_intent,
       coalesce(steer_target_turn_id,'') as steer_target_turn_id,admitted_at,refresh_skills
from dorf.job_messages
where id=sqlc.arg(message_id);

-- name: GetActiveAgentTurn :one
select coalesce(turn_id,'') as turn_id,coalesce(harness,'') as harness,
       coalesce(thread_id,'') as thread_id
from dorf.agent_runs ar
where ar.job_id=sqlc.arg(job_id) and ar.state='active' and ar.turn_id is not null
  and not ar.interrupt_requested
  and ar.role=sqlc.arg(role) and ar.sandbox_id=sqlc.arg(sandbox_id)
  and (
    select count(*) from dorf.agent_runs active
    where active.job_id=sqlc.arg(job_id) and active.state='active' and active.turn_id is not null
      and active.role=sqlc.arg(role) and active.sandbox_id=sqlc.arg(sandbox_id)
  )=1;

-- name: GetLatestAgentRun :one
select state,coalesce(turn_outcome,'') as turn_outcome,
       coalesce(harness,'') as harness,coalesce(thread_id,'') as thread_id
from dorf.agent_runs ar
join dorf.job_messages m on m.id=ar.message_id
where ar.job_id=sqlc.arg(job_id) and ar.role=sqlc.arg(role)
order by m.sequence desc
limit 1;

-- name: NextMessageSequence :one
select (coalesce(max(sequence),0)+1)::bigint
from dorf.job_messages
where job_id=sqlc.arg(job_id);

-- name: InsertMessage :exec
insert into dorf.job_messages(
    id,job_id,from_kind,from_id,sequence,input,delivery_intent,steer_target_turn_id,requested_intent,refresh_skills
)
values(
    sqlc.arg(id),sqlc.arg(job_id),sqlc.arg(from_kind),sqlc.arg(from_id),
    sqlc.arg(sequence),sqlc.arg(input),sqlc.arg(delivery_intent),
    nullif(sqlc.arg(steer_target_turn_id)::text,''),sqlc.arg(requested_intent),sqlc.arg(refresh_skills)
);

-- name: GetFirstUnsettledInput :one
select m.sequence,coalesce(ar.state,'') as state,coalesce(ar.attention,'') as attention
from dorf.job_messages m
left join dorf.agent_runs ar on ar.message_id=m.id
where m.job_id=sqlc.arg(job_id)
  and ar.state not in ('completed','failed','interrupted')
order by m.sequence
limit 1;

-- name: CountUnsettledInputs :one
select count(*)
from dorf.job_messages m
left join dorf.agent_runs ar on ar.message_id=m.id
where m.job_id=sqlc.arg(job_id)
  and ar.state not in ('completed','failed','interrupted');

-- name: GetLatestTurnStartRun :one
select ar.id,ar.job_id,ar.state,ar.role,coalesce(ar.input_revision,'') as input_revision,
       exists (
           select 1 from dorf.evidence e
           where e.agent_run_id=ar.id and e.kind='git-revision'
       ) as observed
from dorf.job_messages m
join dorf.agent_runs ar on ar.message_id=m.id
where m.job_id=sqlc.arg(job_id) and ar.role='implement'
  and m.delivery_intent='follow'
order by m.sequence desc
limit 1;

-- name: ListDeliveries :many
select m.id as message_id,m.job_id as message_job_id,m.from_kind,m.from_id,m.sequence,m.input,m.delivery_intent,
       coalesce(m.steer_target_turn_id,'') as steer_target_turn_id,m.refresh_skills,
       m.admitted_at,
       (ar.id is not null)::boolean as agent_run_present,
       coalesce(ar.id,'') as agent_run_id,coalesce(ar.job_id,'') as agent_run_job_id,
       coalesce(ar.message_id,'') as agent_run_message_id,coalesce(ar.state,'') as state,
       coalesce(ar.harness,'') as harness,coalesce(ar.thread_id,'') as thread_id,
       (ar.baseline_turn_id is not null)::boolean as baseline_recorded,
       coalesce(ar.baseline_turn_id,'') as baseline_turn_id,coalesce(ar.turn_id,'') as turn_id,
       coalesce(ar.turn_outcome,'') as turn_outcome,
       coalesce(ar.attention,'') as attention,coalesce(ar.role,'') as role,coalesce(ar.input_revision,'') as input_revision,
       coalesce(ar.capability,'') as capability,coalesce(ar.sandbox_id,'') as sandbox_id,
       coalesce(ar.submission_nonce,'') as submission_nonce,ar.started_at,ar.finished_at,
       exists (
           select 1 from dorf.agent_runs source
           where source.job_id=ar.job_id and source.sandbox_id=ar.sandbox_id
             and source.harness=ar.harness and source.thread_id=ar.thread_id
             and source.turn_id=coalesce(ar.turn_id,m.steer_target_turn_id)
             and source.interrupt_requested
       ) as interrupt_requested
from dorf.job_messages m
left join dorf.agent_runs ar on ar.message_id=m.id
where m.job_id=sqlc.arg(job_id)
order by m.sequence;

-- name: NextWakeSequence :one
select coalesce(
    (
        select min(m.sequence)
        from dorf.job_messages m
        join dorf.agent_runs ar on ar.message_id=m.id
        where m.job_id=sqlc.arg(job_id)
          and ar.state='pending' and ar.turn_id is null
          and not exists (
              select 1
              from dorf.job_messages earlier
              join dorf.agent_runs earlier_run on earlier_run.message_id=earlier.id
              where earlier.job_id=m.job_id and earlier.sequence<m.sequence
                and earlier_run.state not in ('completed','failed','interrupted')
          )
    ),
    (select coalesce(max(sequence),0)+1 from dorf.job_messages where job_id=sqlc.arg(job_id))
)::bigint;

-- name: AgentMessageNeedsSkillRefresh :one
with current_message as (
    select m.id,m.job_id,m.sequence,m.delivery_intent,ar.sandbox_id,ar.role
    from dorf.job_messages m join dorf.agent_runs ar on ar.message_id=m.id
    where m.id=sqlc.arg(message_id)
), previous_turn as (
    select m.sequence,ar.turn_id
    from current_message current
    join dorf.job_messages m on m.job_id=current.job_id and m.sequence<current.sequence
    join dorf.agent_runs ar on ar.message_id=m.id
    where m.delivery_intent='follow' and ar.turn_id is not null
      and ar.sandbox_id=current.sandbox_id and ar.role=current.role
    order by m.sequence desc limit 1
)
select exists (
    select 1
    from current_message current
    join dorf.job_messages requested on requested.job_id=current.job_id
    join dorf.agent_runs request_run on request_run.message_id=requested.id
    where current.delivery_intent='follow' and requested.refresh_skills
      and request_run.sandbox_id=current.sandbox_id and request_run.role=current.role
      and (
        (requested.delivery_intent='follow' and requested.sequence<=current.sequence
          and requested.sequence>coalesce((select sequence from previous_turn),0))
        or (requested.delivery_intent='steer'
          and requested.steer_target_turn_id=(select turn_id from previous_turn))
      )
) as refresh_skills;
