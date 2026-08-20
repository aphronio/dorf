-- name: GetOutcome :one
select job_id,outcome,coalesce(observed_state,'') as observed_state,observed_merged,
       coalesce(merge_commit_oid,'') as merge_commit_oid,observed_at
from dorf.job_outcomes
where job_id=sqlc.arg(job_id);

-- name: GetOutcomeJobForUpdate :one
select c.revision,j.admission_open,j.cleanup_state
from dorf.jobs j
join dorf.coding_to_proposal_inputs c on c.job_id=j.id
where j.id=sqlc.arg(job_id)
for update of j,c;

-- name: OutcomeImplementationSettled :one
select (
  not exists (
    select 1 from dorf.agent_runs ar
    where ar.job_id=sqlc.arg(job_id) and ar.role='implement'
      and ar.state not in ('completed','failed','interrupted')
  )
  and coalesce((
    select ar.state='completed'
    from dorf.agent_runs ar
    join dorf.job_messages m on m.id=ar.message_id
    where ar.job_id=sqlc.arg(job_id) and ar.role='implement'
    order by m.sequence desc
    limit 1
  ),false)
  and coalesce((
    select ar.state='completed' and exists (
      select 1 from dorf.evidence e
      where e.agent_run_id=ar.id and e.kind='git-revision' and e.revision=c.revision
    )
    from dorf.agent_runs ar
    join dorf.job_messages m on m.id=ar.message_id
    join dorf.coding_to_proposal_inputs c on c.job_id=ar.job_id
    where ar.job_id=sqlc.arg(job_id) and ar.role='implement'
      and (m.delivery_intent='follow' or (
        m.delivery_intent='steer' and ar.turn_id is not null and ar.turn_id<>m.steer_target_turn_id
      ))
    order by m.sequence desc
    limit 1
  ),false)
)::boolean as settled;

-- name: OutcomePublicationIntentExists :one
select exists(
  select 1 from dorf.actions
  where job_id=sqlc.arg(job_id) and kind='github-pull-request'
)::boolean;

-- name: CloseAdmissionForOutcome :execrows
update dorf.jobs
set admission_open=false
where id=sqlc.arg(job_id) and admission_open and cleanup_state='pending';

-- name: InsertOutcome :one
insert into dorf.job_outcomes(
    job_id,outcome,observed_state,observed_merged,merge_commit_oid,observed_at
) values(
    sqlc.arg(job_id),sqlc.arg(outcome),nullif(sqlc.arg(observed_state)::text,''),
    sqlc.arg(observed_merged),nullif(sqlc.arg(merge_commit_oid)::text,''),sqlc.arg(observed_at)
)
returning job_id,outcome,coalesce(observed_state,'') as observed_state,observed_merged,
          coalesce(merge_commit_oid,'') as merge_commit_oid,observed_at;
