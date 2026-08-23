-- name: NextCodingAgentMessage :one
with current_turn_start as (
    select m.id as message_id,ar.turn_id,ar.state
    from dorf.job_messages m join dorf.agent_runs ar on ar.message_id=m.id
    where m.job_id=sqlc.arg(job_id) and ar.turn_id is not null
      and ar.role='implement'
      and (m.delivery_intent='follow' or ar.turn_id<>m.steer_target_turn_id)
      and ar.state in ('active','uncertain')
    order by m.sequence limit 1
), current_unbound_mutation as (
    select m.id as message_id,m.sequence
    from dorf.job_messages m join dorf.agent_runs ar on ar.message_id=m.id
    where m.job_id=sqlc.arg(job_id) and ar.role='implement'
      and ar.turn_id is null and ar.state in ('submitting','uncertain')
    order by m.sequence limit 1
), candidate as (
    select steer.id as message_id,0 as priority,steer.sequence
    from current_turn_start active
    join dorf.job_messages steer on steer.job_id=sqlc.arg(job_id) and steer.delivery_intent='steer'
      and steer.steer_target_turn_id=active.turn_id
    join dorf.agent_runs steer_run on steer_run.message_id=steer.id
    where active.state='active' and steer_run.role='implement'
      and steer_run.state in ('pending','submitting') and steer_run.turn_id is null
    union all
    select message_id,1,0 from current_turn_start
    union all
    select message_id,1,sequence from current_unbound_mutation
    where not exists(select 1 from current_turn_start)
    union all
    select m.id,2,m.sequence
    from dorf.job_messages m join dorf.agent_runs ar on ar.message_id=m.id
    where m.job_id=sqlc.arg(job_id) and ar.role='implement'
      and ar.state in ('pending','submitting')
      and ar.turn_id is null
      and not exists(select 1 from current_turn_start)
      and not exists(select 1 from current_unbound_mutation)
)
select m.id,m.job_id,m.from_kind,m.from_id,m.sequence,m.input,m.delivery_intent,
       coalesce(m.steer_target_turn_id,'') as steer_target_turn_id,m.admitted_at
from candidate c join dorf.job_messages m on m.id=c.message_id
order by c.priority,c.sequence limit 1;
