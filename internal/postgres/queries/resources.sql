-- name: GetSandbox :one
select id,job_id,name,ownership_nonce
from dorf.sandboxes
where id=sqlc.arg(id);

-- name: GetSandboxForUpdate :one
select id,job_id,name,ownership_nonce
from dorf.sandboxes
where id=sqlc.arg(id)
for update;

-- name: GetJobSandboxByNameForUpdate :one
select id,job_id,name,ownership_nonce
from dorf.sandboxes
where job_id=sqlc.arg(job_id) and name=sqlc.arg(name)
for update;

-- name: ListJobSandboxes :many
select s.id,s.job_id,s.name,s.ownership_nonce
from dorf.sandboxes s
where s.job_id=sqlc.arg(job_id)
order by s.id;
