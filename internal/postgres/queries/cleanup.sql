-- name: GetCleanupJobForUpdate :one
select admission_open,cleanup_state,coalesce(cleanup_task_id,'') as cleanup_task_id,
       coalesce(workflow_attention,'') as workflow_attention
from dorf.jobs
where id=sqlc.arg(job_id)
for update;

-- name: CountUnsettledJobResources :one
select count(*)
from dorf.sandboxes s
left join dorf.routes r on r.sandbox_id=s.id
where s.job_id=sqlc.arg(job_id)
  and (s.state<>'deleted' or (r.id is not null and r.state<>'revoked'));

-- name: GetResourceStates :one
select s.state as sandbox_state,coalesce(r.state,'') as route_state
from dorf.sandboxes s
left join dorf.routes r on r.sandbox_id=s.id
where s.id=sqlc.arg(sandbox_id) and s.job_id=sqlc.arg(job_id);

-- name: CompleteCleanup :execrows
update dorf.jobs
set cleanup_state='complete',cleanup_attention=null,workflow_attention=null,
    cleaned_at=coalesce(cleaned_at,clock_timestamp())
where id=sqlc.arg(job_id) and cleanup_state='scheduled';
