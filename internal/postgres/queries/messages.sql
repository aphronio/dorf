-- name: GetMessageBySender :one
select id,session_id,from_kind,from_id,sequence,input,attachments,delivery_intent,requested_intent,
       coalesce(steer_target_turn_id,'') as steer_target_turn_id,admitted_at,refresh_skills,developer_instructions,observation
from dorf.session_messages
where session_id=sqlc.arg(session_id) and from_kind=sqlc.arg(from_kind)
  and from_id=sqlc.arg(from_id);

-- name: GetMessage :one
select id,session_id,from_kind,from_id,sequence,input,attachments,delivery_intent,
       requested_intent,coalesce(steer_target_turn_id,'') as steer_target_turn_id,admitted_at,refresh_skills,developer_instructions,observation
from dorf.session_messages
where id=sqlc.arg(message_id);

-- name: GetActiveAgentTurn :one
select coalesce(turn_id,'') as turn_id,coalesce(harness,'') as harness,
       coalesce(thread_id,'') as thread_id
from dorf.agent_runs ar
where ar.session_id=sqlc.arg(session_id) and ar.state='active' and ar.turn_id is not null
  and not ar.interrupt_requested
  and ar.sandbox_id=sqlc.arg(sandbox_id)
  and (
    select count(*) from dorf.agent_runs active
    where active.session_id=sqlc.arg(session_id) and active.state='active' and active.turn_id is not null
      and active.sandbox_id=sqlc.arg(sandbox_id)
  )=1;

-- name: NextMessageSequence :one
select (coalesce(max(sequence),0)+1)::bigint
from dorf.session_messages
where session_id=sqlc.arg(session_id);

-- name: InsertMessage :exec
insert into dorf.session_messages(
    id,session_id,from_kind,from_id,sequence,input,attachments,delivery_intent,steer_target_turn_id,requested_intent,refresh_skills,developer_instructions,observation
)
values(
    sqlc.arg(id),sqlc.arg(session_id),sqlc.arg(from_kind),sqlc.arg(from_id),
    sqlc.arg(sequence),sqlc.arg(input),sqlc.arg(attachments)::jsonb,sqlc.arg(delivery_intent),
    nullif(sqlc.arg(steer_target_turn_id)::text,''),sqlc.arg(requested_intent),sqlc.arg(refresh_skills),sqlc.narg(developer_instructions),sqlc.arg(observation)
);

-- name: ListDeliveries :many
select m.id as message_id,m.session_id as message_session_id,m.from_kind,m.from_id,m.sequence,m.input,m.attachments,m.delivery_intent,
       m.requested_intent,coalesce(m.steer_target_turn_id,'') as steer_target_turn_id,m.refresh_skills,m.developer_instructions,m.observation,
       m.admitted_at,
       (ar.id is not null)::boolean as agent_run_present,
       coalesce(ar.id,'') as agent_run_id,coalesce(ar.session_id,'') as agent_run_session_id,
       coalesce(ar.message_id,'') as agent_run_message_id,coalesce(ar.state,'') as state,
       coalesce(ar.harness,'') as harness,coalesce(ar.thread_id,'') as thread_id,
       (ar.baseline_turn_id is not null)::boolean as baseline_recorded,
       coalesce(ar.baseline_turn_id,'') as baseline_turn_id,coalesce(ar.turn_id,'') as turn_id,
       coalesce(ar.turn_outcome,'') as turn_outcome,
       coalesce(ar.attention,'') as attention,
       coalesce(ar.sandbox_id,'') as sandbox_id,
       ar.started_at,ar.finished_at,
       -- Uncorrelated membership lets generic prepared plans hash identities once
       -- instead of scanning the Session's runs again for every Delivery.
       case when ar.harness is not null and ar.thread_id is not null
         and coalesce(ar.turn_id,m.steer_target_turn_id) is not null then
           (ar.session_id,ar.sandbox_id,ar.harness,ar.thread_id,coalesce(ar.turn_id,m.steer_target_turn_id)) in (
               select source.session_id,source.sandbox_id,source.harness,source.thread_id,source.turn_id
               from dorf.agent_runs source
               where source.session_id=sqlc.arg(session_id) and source.interrupt_requested
           )
         else false
       end as interrupt_requested
from dorf.session_messages m
left join dorf.agent_runs ar on ar.message_id=m.id
where m.session_id=sqlc.arg(session_id)
order by m.sequence;

-- name: AgentMessageNeedsSkillRefresh :one
with current_message as (
    select m.id,m.session_id,m.sequence,m.delivery_intent,ar.sandbox_id
    from dorf.session_messages m join dorf.agent_runs ar on ar.message_id=m.id
    where m.id=sqlc.arg(message_id)
), previous_turn as (
    select m.sequence,ar.turn_id
    from current_message current
    join dorf.session_messages m on m.session_id=current.session_id and m.sequence<current.sequence
    join dorf.agent_runs ar on ar.message_id=m.id
    where m.delivery_intent='follow' and ar.turn_id is not null
      and ar.sandbox_id=current.sandbox_id
    order by m.sequence desc limit 1
)
select exists (
    select 1
    from current_message current
    join dorf.session_messages requested on requested.session_id=current.session_id
    join dorf.agent_runs request_run on request_run.message_id=requested.id
    where current.delivery_intent='follow' and requested.refresh_skills
      and request_run.sandbox_id=current.sandbox_id
      and (
        (requested.delivery_intent='follow' and requested.sequence<=current.sequence
          and requested.sequence>coalesce((select sequence from previous_turn),0))
        or (requested.delivery_intent='steer'
          and requested.steer_target_turn_id=(select turn_id from previous_turn))
      )
) as refresh_skills;
