-- name: GetOutcome :one
select job_id,outcome,observed_state,observed_merged,
       coalesce(merge_commit_oid,'') as merge_commit_oid,observed_at
from dorf.job_outcomes
where job_id=sqlc.arg(job_id);

-- name: GetOutcomeJobForUpdate :one
select revision,workflow_phase
from dorf.jobs
where id=sqlc.arg(job_id)
for update;

-- name: InsertOutcome :one
insert into dorf.job_outcomes(
    job_id,outcome,observed_state,observed_merged,merge_commit_oid,observed_at
) values(
    sqlc.arg(job_id),sqlc.arg(outcome),sqlc.arg(observed_state),
    sqlc.arg(observed_merged),nullif(sqlc.arg(merge_commit_oid)::text,''),sqlc.arg(observed_at)
)
returning job_id,outcome,observed_state,observed_merged,
          coalesce(merge_commit_oid,'') as merge_commit_oid,observed_at;

-- name: ClearOutcomeAttention :execrows
update dorf.jobs
set workflow_attention=null
where id=sqlc.arg(job_id) and revision=sqlc.arg(revision) and workflow_phase='published';
