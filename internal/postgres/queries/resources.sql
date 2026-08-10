-- name: GetSandbox :one
select id,job_id,ownership_nonce
from dorf.sandboxes
where id=sqlc.arg(id);

-- name: ListJobSandboxes :many
select s.id,s.job_id,s.ownership_nonce
from dorf.sandboxes s
where s.job_id=sqlc.arg(job_id)
order by s.id;
