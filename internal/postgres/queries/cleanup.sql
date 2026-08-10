-- name: GetCleanupJobForUpdate :one
select admission_open,cleanup_state,coalesce(cleanup_task_id,'') as cleanup_task_id,
       coalesce(workflow_attention,'') as workflow_attention
from dorf.jobs
where id=sqlc.arg(job_id)
for update;

-- name: CountUnsettledReviewResources :one
select count(*)
from dorf.review_resources
where job_id=sqlc.arg(job_id)
  and (route_state<>'revoked' or sandbox_state<>'deleted');

-- name: GetMainResourceStates :one
select coalesce((select sb.state from dorf.sandboxes sb where sb.job_id=sqlc.arg(job_id)),'') as sandbox_state,
       coalesce((select r.state from dorf.routes r where r.job_id=sqlc.arg(job_id)),'') as route_state;

-- name: CompleteCleanup :execrows
update dorf.jobs
set cleanup_state='complete',cleanup_attention=null,workflow_attention=null,
    cleaned_at=coalesce(cleaned_at,clock_timestamp())
where id=sqlc.arg(job_id) and cleanup_state='scheduled';
