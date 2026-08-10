-- name: GetSandbox :one
select id,job_id,state,ownership_nonce
from dorf.sandboxes
where id=sqlc.arg(id);

-- name: GetRouteBySandbox :one
select id,sandbox_id,state
from dorf.routes
where sandbox_id=sqlc.arg(sandbox_id);

-- name: ListJobSandboxes :many
select s.id,s.job_id,s.state,s.ownership_nonce,
       coalesce(r.id,'') as route_id,coalesce(r.state,'') as route_state
from dorf.sandboxes s
left join dorf.routes r on r.sandbox_id=s.id
where s.job_id=sqlc.arg(job_id)
order by s.id;

-- name: GetScopedActionBySandbox :one
select id,job_id,kind,state,coalesce(external_id,'') as external_id,
       coalesce(external_outcome,'') as external_outcome,scope_key
from dorf.actions
where job_id=sqlc.arg(job_id) and kind=sqlc.arg(kind) and scope_key=sqlc.arg(sandbox_id);
