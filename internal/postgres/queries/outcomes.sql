-- name: GetOutcome :one
select job_id,outcome,observed_state,observed_merged,
       coalesce(merge_commit_oid,'') as merge_commit_oid,observed_at
from dorf.job_outcomes
where job_id=sqlc.arg(job_id);

-- name: GetOutcomeJobForUpdate :one
select revision,admission_open,cleanup_state
from dorf.jobs
where id=sqlc.arg(job_id)
for update;

-- name: OutcomeImplementationSettled :one
select (
  not exists (
    select 1 from dorf.agent_runs ar
    where ar.job_id=sqlc.arg(job_id) and ar.role='implement'
      and ar.state not in ('completed','failed','interrupted')
  )
  and coalesce((
    select ar.state='completed' and exists (
      select 1 from dorf.evidence e
      where e.agent_run_id=ar.id and e.kind='git-revision' and e.revision=j.revision
    )
    from dorf.agent_runs ar
    join dorf.job_messages m on m.id=ar.message_id
    join dorf.jobs j on j.id=ar.job_id
    where ar.job_id=sqlc.arg(job_id) and ar.role='implement' and m.delivery_intent='follow'
    order by m.sequence desc
    limit 1
  ),false)
)::boolean as settled;

-- name: InsertOutcome :one
insert into dorf.job_outcomes(
    job_id,outcome,observed_state,observed_merged,merge_commit_oid,observed_at
) values(
    sqlc.arg(job_id),sqlc.arg(outcome),sqlc.arg(observed_state),
    sqlc.arg(observed_merged),nullif(sqlc.arg(merge_commit_oid)::text,''),sqlc.arg(observed_at)
)
returning job_id,outcome,observed_state,observed_merged,
          coalesce(merge_commit_oid,'') as merge_commit_oid,observed_at;
