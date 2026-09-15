-- name: GetSandbox :one
select s.id,s.job_id,s.name,r.ownership_nonce,s.active_resource_id,coalesce(r.provider_id,'') as provider_id
from dorf.sandboxes s join dorf.sandbox_resources r on r.id=s.active_resource_id
where s.id=sqlc.arg(id);

-- name: GetSandboxForUpdate :one
select s.id,s.job_id,s.name,r.ownership_nonce,s.active_resource_id,coalesce(r.provider_id,'') as provider_id
from dorf.sandboxes s join dorf.sandbox_resources r on r.id=s.active_resource_id
where s.id=sqlc.arg(id)
for update;

-- name: GetJobSandboxByNameForUpdate :one
select s.id,s.job_id,s.name,r.ownership_nonce,s.active_resource_id,coalesce(r.provider_id,'') as provider_id
from dorf.sandboxes s join dorf.sandbox_resources r on r.id=s.active_resource_id
where s.job_id=sqlc.arg(job_id) and s.name=sqlc.arg(name)
for update;

-- name: ListJobSandboxes :many
select s.id,s.job_id,s.name,r.ownership_nonce,s.active_resource_id,coalesce(r.provider_id,'') as provider_id
from dorf.sandboxes s join dorf.sandbox_resources r on r.id=s.active_resource_id
where s.job_id=sqlc.arg(job_id)
order by s.id;

-- name: ListSandboxResources :many
select r.id,r.sandbox_id,r.ownership_nonce,coalesce(r.provider_id,'') as provider_id,
       r.reserved_at,r.observed_at,r.deleted_at
from dorf.sandbox_resources r join dorf.sandboxes s on s.id=r.sandbox_id
where s.job_id=sqlc.arg(job_id)
order by r.reserved_at,r.id;

-- name: BindSandboxResource :execrows
update dorf.sandbox_resources r
set provider_id=sqlc.arg(provider_id)::text,observed_at=coalesce(r.observed_at,clock_timestamp())
from dorf.sandboxes s
where s.id=r.sandbox_id and s.id=sqlc.arg(sandbox_id) and s.job_id=sqlc.arg(job_id)
  and s.active_resource_id=r.id and r.id=sqlc.arg(resource_id)
  and r.ownership_nonce=sqlc.arg(ownership_nonce) and r.deleted_at is null
  and (r.provider_id is null or r.provider_id=sqlc.arg(provider_id)::text);

-- name: RecordSandboxResourceDeleted :execrows
update dorf.sandbox_resources
set deleted_at=coalesce(deleted_at,clock_timestamp())
where id=sqlc.arg(resource_id) and sandbox_id=sqlc.arg(sandbox_id);
