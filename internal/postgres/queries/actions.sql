-- name: ReserveSandbox :execrows
with reserved as (
    insert into dorf.sandboxes(id,job_id,name,active_resource_id)
    values(sqlc.arg(id),sqlc.arg(job_id),sqlc.arg(name),sqlc.arg(id)::text || ':initial')
    on conflict do nothing returning id,active_resource_id
)
insert into dorf.sandbox_resources(id,sandbox_id,ownership_nonce)
select active_resource_id,id,sqlc.arg(ownership_nonce) from reserved;

-- name: GetScopedAction :one
select id,job_id,kind,state,scope_key,created_at,settled_at
from dorf.actions
where job_id=sqlc.arg(job_id) and kind=sqlc.arg(kind) and scope_key=sqlc.arg(scope_key);

-- name: InsertScopedAction :execrows
insert into dorf.actions(id,job_id,kind,state,scope_key)
values(sqlc.arg(id),sqlc.arg(job_id),sqlc.arg(kind),'unsettled',sqlc.arg(scope_key))
on conflict do nothing;

-- name: GetActionByIDForUpdate :one
select id,job_id,kind,state,scope_key,created_at,settled_at
from dorf.actions
where id=sqlc.arg(id)
for update;

-- name: RecordSandboxActionSuccess :execrows
update dorf.actions
set state='succeeded',settled_at=coalesce(settled_at,clock_timestamp())
where id=sqlc.arg(id) and state<>'succeeded';

-- name: ListActions :many
select a.id,a.job_id,a.kind,a.state,a.scope_key,a.created_at,a.settled_at
from dorf.actions a
where a.job_id=sqlc.arg(job_id)
order by a.created_at,a.id;
