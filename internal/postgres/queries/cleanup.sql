-- name: GetCleanupJobForUpdate :one
select admission_open,cleanup_state,
       coalesce((select task_id from dorf.job_tasks where job_id=dorf.jobs.id order by sequence desc limit 1),'') as current_task_id,
       coalesce(cleanup_attention,'') as cleanup_attention
from dorf.jobs
where id=sqlc.arg(job_id)
for update;

-- name: RequestCleanup :execrows
update dorf.jobs
set admission_open=false,
    cleanup_state=case when cleanup_state='pending' then 'requested' else cleanup_state end
where id=sqlc.arg(job_id) and cleanup_state in ('pending','requested');

-- name: ListCleanupRequests :many
select id from dorf.jobs where not admission_open and cleanup_state='requested' order by id;

-- name: CountUnsettledSandboxCleanupActions :one
select count(*)
from dorf.sandboxes s
where s.job_id=sqlc.arg(job_id)
  and (
    not exists(select 1 from dorf.actions a where a.job_id=s.job_id and a.kind='provider-route-revoke' and a.scope_key=s.id and a.state='succeeded')
    or not exists(select 1 from dorf.actions a where a.job_id=s.job_id and a.kind='sandbox-delete' and a.scope_key=s.id and a.state='succeeded')
  );

-- name: CompleteCleanup :execrows
update dorf.jobs
set cleanup_state='complete',cleanup_attention=null,
    workflow_attention=null,workflow_attention_source=null,workflow_attention_at=null,
    cleaned_at=coalesce(cleaned_at,clock_timestamp())
where id=sqlc.arg(job_id) and cleanup_state='scheduled';
