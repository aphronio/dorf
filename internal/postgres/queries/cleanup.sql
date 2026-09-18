-- name: GetCleanupSessionForUpdate :one
select admission_open,cleanup_state,
       coalesce((select task_id from dorf.session_tasks where session_id=dorf.sessions.id order by sequence desc limit 1),'') as current_task_id,
       coalesce(cleanup_attention,'') as cleanup_attention
from dorf.sessions
where id=sqlc.arg(session_id)
for update;

-- name: RequestCleanup :execrows
update dorf.sessions
set admission_open=false,
    cleanup_state=case when cleanup_state='pending' then 'requested' else cleanup_state end
where id=sqlc.arg(session_id) and cleanup_state in ('pending','requested');

-- name: CountUnsettledSandboxCleanupActions :one
select count(*)
from dorf.sandboxes s
where s.session_id=sqlc.arg(session_id)
  and (
    not exists(select 1 from dorf.actions a where a.session_id=s.session_id and a.kind='provider-route-revoke' and a.scope_key=s.id and a.state='succeeded')
    or not exists(select 1 from dorf.actions a where a.session_id=s.session_id and a.kind='sandbox-delete' and a.scope_key=s.id and a.state='succeeded')
    or exists(select 1 from dorf.sandbox_resources r where r.sandbox_id=s.id and r.deleted_at is null)
    or exists(select 1 from dorf.sandbox_upgrades u where u.sandbox_id=s.id and u.checkpoint_reference is not null and u.checkpoint_deleted_at is null)
  );

-- name: CompleteCleanup :one
with completed as (
    update dorf.sessions j0
    set cleanup_state='complete',cleanup_attention=null,
        execution_attention=null,execution_attention_source=null,execution_attention_at=null,
        cleaned_at=coalesce(cleaned_at,clock_timestamp())
    where j0.id=sqlc.arg(session_id) and j0.cleanup_state='scheduled'
    returning j0.id
), released as (
    update dorf.sandbox_delivery_holds h set released_at=clock_timestamp()
    from dorf.sandboxes s join completed j on j.id=s.session_id
    where h.sandbox_id=s.id and h.released_at is null
)
select count(*) from completed;
