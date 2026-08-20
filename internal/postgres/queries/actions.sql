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
select id,job_id,kind,state,scope_key,created_at,settled_at
from dorf.actions
where id=sqlc.arg(id) and job_id=sqlc.arg(job_id) and kind=sqlc.arg(kind)
for update;

-- name: GetAction :one
select id,job_id,kind,state,scope_key,created_at,settled_at
from dorf.actions
where id=sqlc.arg(id) and job_id=sqlc.arg(job_id) and kind=sqlc.arg(kind);

-- name: GetScopedAction :one
select id,job_id,kind,state,scope_key,created_at,settled_at
from dorf.actions
where job_id=sqlc.arg(job_id) and kind=sqlc.arg(kind) and scope_key=sqlc.arg(scope_key);

-- name: InsertScopedAction :execrows
insert into dorf.actions(id,job_id,kind,state,scope_key)
values(sqlc.arg(id),sqlc.arg(job_id),sqlc.arg(kind),'unsettled',sqlc.arg(scope_key))
on conflict do nothing;

-- name: GetSetupActionForUpdate :one
select a.job_id,a.kind,coalesce(c.setup_action_id,'') as setup_action_id
from dorf.actions a
join dorf.coding_to_proposal_inputs c on c.job_id=a.job_id
where a.id=sqlc.arg(action_id)
for update of a,c;

-- name: FinishSetupAction :execrows
update dorf.actions
set state=sqlc.arg(state),settled_at=coalesce(settled_at,clock_timestamp())
where dorf.actions.id=sqlc.arg(action_id)
  and (state='unsettled' or state=sqlc.arg(state));

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
