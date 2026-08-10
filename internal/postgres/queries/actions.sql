-- name: InsertActionIfAbsent :exec
insert into dorf.actions(id,job_id,kind,state)
values(sqlc.arg(id),sqlc.arg(job_id),sqlc.arg(kind),'unsettled')
on conflict do nothing;

-- name: ReserveSandbox :execrows
insert into dorf.sandboxes(id,job_id,ownership_nonce)
values(sqlc.arg(id),sqlc.arg(job_id),sqlc.arg(ownership_nonce))
on conflict(id) do update set id=dorf.sandboxes.id
where dorf.sandboxes.job_id=excluded.job_id
  and dorf.sandboxes.ownership_nonce=excluded.ownership_nonce;

-- name: GetActionForUpdate :one
select id,job_id,kind,state,scope_key
from dorf.actions
where id=sqlc.arg(id) and job_id=sqlc.arg(job_id) and kind=sqlc.arg(kind)
for update;

-- name: GetAction :one
select id,job_id,kind,state,scope_key
from dorf.actions
where id=sqlc.arg(id) and job_id=sqlc.arg(job_id) and kind=sqlc.arg(kind);

-- name: GetActionStateForUpdate :one
select state
from dorf.actions
where id=sqlc.arg(id) and job_id=sqlc.arg(job_id) and kind=sqlc.arg(kind)
for update;

-- name: InsertScopedAction :execrows
insert into dorf.actions(id,job_id,kind,state,scope_key)
values(sqlc.arg(id),sqlc.arg(job_id),sqlc.arg(kind),'unsettled',sqlc.arg(scope_key))
on conflict do nothing;

-- name: GetSetupActionForUpdate :one
select a.job_id,a.kind,coalesce(j.setup_action_id,'') as setup_action_id
from dorf.actions a
join dorf.jobs j on j.id=a.job_id
where a.id=sqlc.arg(action_id)
for update of a,j;

-- name: FinishSetupAction :exec
update dorf.actions
set state=sqlc.arg(state)
where dorf.actions.id=sqlc.arg(action_id);

-- name: GetActionCompletionForUpdate :one
select job_id,kind,state,scope_key
from dorf.actions
where id=sqlc.arg(id)
for update;

-- name: RecordSandboxActionSuccess :execrows
update dorf.actions
set state='succeeded'
where id=sqlc.arg(id) and state<>'succeeded';

-- name: SandboxRouteRevokeSucceeded :one
select exists(
    select 1 from dorf.actions
    where job_id=sqlc.arg(job_id) and kind='provider-route-revoke'
      and scope_key=sqlc.arg(sandbox_id) and state='succeeded'
);

-- name: ListActions :many
select a.id,a.kind,a.state,a.scope_key,
       coalesce(e.digest,'') as evidence_digest
from dorf.actions a
left join dorf.evidence e on e.action_id=a.id
where a.job_id=sqlc.arg(job_id)
order by a.created_at,a.id;
