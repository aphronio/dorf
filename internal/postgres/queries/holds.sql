-- name: GetSandboxDeliveryHold :one
select id,sandbox_id,reason,requested_at,released_at
from dorf.sandbox_delivery_holds where id=sqlc.arg(id);

-- name: InsertSandboxDeliveryHold :exec
insert into dorf.sandbox_delivery_holds(id,sandbox_id,reason)
values(sqlc.arg(id),sqlc.arg(sandbox_id),'workspace_upgrade');

-- name: SandboxDeliveryHeld :one
select exists(select 1 from dorf.sandbox_delivery_holds
where sandbox_id=sqlc.arg(sandbox_id) and released_at is null)::boolean;

-- name: ReleaseSandboxDeliveryHold :execrows
update dorf.sandbox_delivery_holds set released_at=coalesce(released_at,clock_timestamp())
where id=sqlc.arg(id) and sandbox_id=sqlc.arg(sandbox_id);

-- name: ListJobDeliveryHolds :many
select h.id,h.sandbox_id,h.reason,h.requested_at,h.released_at
from dorf.sandbox_delivery_holds h join dorf.sandboxes s on s.id=h.sandbox_id
where s.job_id=sqlc.arg(job_id) and h.released_at is null
order by h.requested_at,h.id;
