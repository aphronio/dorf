-- name: InsertActionIfAbsent :exec
insert into dorf.actions(id,job_id,kind,state)
values(sqlc.arg(id),sqlc.arg(job_id),sqlc.arg(kind),'pending')
on conflict do nothing;

-- name: ReserveSandbox :execrows
insert into dorf.sandboxes(id,job_id,state,ownership_nonce)
values(sqlc.arg(id),sqlc.arg(job_id),'pending',sqlc.arg(ownership_nonce))
on conflict(id) do update set id=dorf.sandboxes.id
where dorf.sandboxes.job_id=excluded.job_id
  and dorf.sandboxes.ownership_nonce=excluded.ownership_nonce;

-- name: ReserveRoute :execrows
insert into dorf.routes(id,sandbox_id,state)
values(sqlc.arg(id),sqlc.arg(sandbox_id),'pending')
on conflict(sandbox_id) do update set sandbox_id=dorf.routes.sandbox_id
where dorf.routes.id=excluded.id;

-- name: GetActionForUpdate :one
select id,job_id,kind,state,
       coalesce(external_id,'') as external_id,
       coalesce(external_outcome,'') as external_outcome,scope_key
from dorf.actions
where id=sqlc.arg(id) and job_id=sqlc.arg(job_id) and kind=sqlc.arg(kind)
for update;

-- name: GetAction :one
select id,job_id,kind,state,
       coalesce(external_id,'') as external_id,
       coalesce(external_outcome,'') as external_outcome,scope_key
from dorf.actions
where id=sqlc.arg(id) and job_id=sqlc.arg(job_id) and kind=sqlc.arg(kind);

-- name: GetActionStateForUpdate :one
select state
from dorf.actions
where id=sqlc.arg(id) and job_id=sqlc.arg(job_id) and kind=sqlc.arg(kind)
for update;

-- name: InsertScopedAction :execrows
insert into dorf.actions(id,job_id,kind,state,scope_key)
values(sqlc.arg(id),sqlc.arg(job_id),sqlc.arg(kind),'pending',sqlc.arg(scope_key))
on conflict do nothing;

-- name: GetSetupActionForUpdate :one
select a.job_id,a.kind,coalesce(j.setup_action_id,'') as setup_action_id
from dorf.actions a
join dorf.jobs j on j.id=a.job_id
where a.id=sqlc.arg(action_id)
for update of a,j;

-- name: FinishSetupAction :exec
update dorf.actions
set state=sqlc.arg(state),external_id=sqlc.arg(external_id),
    external_outcome=sqlc.arg(external_outcome)
where dorf.actions.id=sqlc.arg(action_id);

-- name: GetActionCompletionForUpdate :one
select job_id,kind,state,coalesce(external_id,'') as external_id,
       coalesce(external_outcome,'') as external_outcome,scope_key
from dorf.actions
where id=sqlc.arg(id)
for update;

-- name: RecordActionSuccess :execrows
update dorf.actions
set state='succeeded',external_id=sqlc.arg(external_id),
    external_outcome=nullif(sqlc.arg(external_outcome)::text,'')
where id=sqlc.arg(id) and state<>'succeeded';

-- name: MarkActionUncertain :exec
update dorf.actions
set state='uncertain'
where id=sqlc.arg(action_id) and state<>'succeeded';

-- name: MarkSandboxCreated :execrows
update dorf.sandboxes
set state='created'
where id=sqlc.arg(sandbox_id) and state in ('pending','created');

-- name: MarkRouteActive :execrows
update dorf.routes
set state='active'
where id=sqlc.arg(route_id) and sandbox_id=sqlc.arg(sandbox_id)
  and state in ('pending','active');

-- name: MarkRouteRevoked :execrows
update dorf.routes
set state='revoked'
where id=sqlc.arg(route_id) and sandbox_id=sqlc.arg(sandbox_id)
  and state in ('pending','active','revoked');

-- name: MarkSandboxDeleted :execrows
update dorf.sandboxes s
set state='deleted'
where s.id=sqlc.arg(sandbox_id)
  and not exists(select 1 from dorf.routes where sandbox_id=sqlc.arg(sandbox_id) and state<>'revoked')
  and s.state in ('pending','created','deleted');

-- name: ListActions :many
select a.id,a.kind,a.state,
       coalesce(a.external_id,'') as external_id,a.scope_key,
       coalesce(e.digest,'') as evidence_digest
from dorf.actions a
left join dorf.evidence e on e.action_id=a.id
where a.job_id=sqlc.arg(job_id)
order by a.created_at,a.id;

-- name: GetReviewRevisionBySandbox :one
select revision from dorf.agent_runs
where sandbox_id=sqlc.arg(sandbox_id) and revision is not null;
