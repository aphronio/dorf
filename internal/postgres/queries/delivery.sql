-- name: NextAgentMessage :one
with current_turn_start as (
    select m.id as message_id,ar.turn_id,ar.state,ar.interrupt_requested
    from dorf.session_messages m join dorf.agent_runs ar on ar.message_id=m.id
    where m.session_id=sqlc.arg(session_id) and ar.turn_id is not null
      and m.delivery_intent='follow'
      and ar.state in ('active','uncertain')
    order by m.sequence limit 1
), current_unbound_mutation as (
    select m.id as message_id,m.sequence
    from dorf.session_messages m join dorf.agent_runs ar on ar.message_id=m.id
    where m.session_id=sqlc.arg(session_id) and m.delivery_intent='follow'
      and ar.turn_id is null and ar.state in ('submitting','uncertain')
    order by m.sequence limit 1
), unsettled_steer as (
    select m.id as message_id,m.sequence
    from dorf.session_messages m join dorf.agent_runs ar on ar.message_id=m.id
    where m.session_id=sqlc.arg(session_id) and m.delivery_intent='steer'
      and ar.state in ('pending','submitting','uncertain') and ar.turn_id is null
    order by m.sequence limit 1
), candidate as (
    select message_id,-1 as priority,0 as sequence from current_turn_start where interrupt_requested
    union all
    select message_id,0 as priority,sequence from unsettled_steer
    union all
    select message_id,1,0 from current_turn_start
    where not exists(select 1 from unsettled_steer)
    union all
    select message_id,1,sequence from current_unbound_mutation
    where not exists(select 1 from unsettled_steer)
      and not exists(select 1 from current_turn_start)
    union all
    select m.id,2,m.sequence
    from dorf.session_messages m join dorf.agent_runs ar on ar.message_id=m.id
    where m.session_id=sqlc.arg(session_id) and m.delivery_intent='follow'
      and ar.state in ('pending','submitting')
      and ar.turn_id is null
      and not exists(select 1 from dorf.sandbox_delivery_holds h where h.sandbox_id=ar.sandbox_id and h.released_at is null)
      and not exists(select 1 from unsettled_steer)
      and not exists(select 1 from current_turn_start)
      and not exists(select 1 from current_unbound_mutation)
)
select m.id,m.session_id,m.from_kind,m.from_id,m.sequence,m.input,m.attachments,m.delivery_intent,
       m.requested_intent,coalesce(m.steer_target_turn_id,'') as steer_target_turn_id,m.admitted_at,m.refresh_skills,m.developer_instructions,m.observation
from candidate c join dorf.session_messages m on m.id=c.message_id
order by c.priority,c.sequence limit 1;
