-- name: GetCheckpointBranchByID :one
select b.*,h.released_at
from dorf.sandbox_branches b
join dorf.sandbox_delivery_holds h on h.id=b.id
where b.id=sqlc.arg(id);

-- name: GetCheckpointBranchBySession :one
select b.*,h.released_at
from dorf.sandbox_branches b
join dorf.sandbox_delivery_holds h on h.id=b.id
where b.destination_session_id=sqlc.arg(destination_session_id);

-- name: InsertCheckpointBranchHold :exec
insert into dorf.sandbox_delivery_holds(id,sandbox_id,reason)
values(sqlc.arg(id),sqlc.arg(destination_sandbox_id),'checkpoint_branch');

-- name: InsertCheckpointBranch :exec
insert into dorf.sandbox_branches(
    id,source_session_id,destination_session_id,destination_sandbox_id,
    checkpoint_repository,checkpoint_snapshot_id,thread_id
) values (
    sqlc.arg(id),sqlc.arg(source_session_id),sqlc.arg(destination_session_id),
    sqlc.arg(destination_sandbox_id),sqlc.arg(checkpoint_repository),
    sqlc.arg(checkpoint_snapshot_id),sqlc.arg(thread_id)
);

-- name: BindCheckpointBranchThread :execrows
update dorf.sessions set thread_id=sqlc.arg(thread_id),native_revision=sqlc.arg(native_revision)
where id=sqlc.arg(session_id) and thread_id is null and native_revision=0
  and native_pending_input_id is null and native_pending_turn_id is null;

-- name: RecordCheckpointBranchRestored :execrows
update dorf.sandbox_branches
set restored_at=coalesce(restored_at,clock_timestamp())
where id=sqlc.arg(id) and ready_at is null;

-- name: RequestCheckpointBranchRelease :execrows
update dorf.sandbox_branches
set release_requested_at=coalesce(release_requested_at,clock_timestamp())
where id=sqlc.arg(id) and restored_at is not null;

-- name: FinishCheckpointBranch :execrows
with ready as (
    update dorf.sandbox_branches b
    set ready_at=coalesce(ready_at,clock_timestamp())
    where b.id=sqlc.arg(id) and b.restored_at is not null and b.release_requested_at is not null
    returning b.id,b.destination_sandbox_id
)
update dorf.sandbox_delivery_holds h
set released_at=coalesce(released_at,clock_timestamp())
from ready
where h.id=ready.id and h.sandbox_id=ready.destination_sandbox_id
  and h.reason='checkpoint_branch';

-- name: CheckpointBranchFilePreparationAllowed :one
select exists(
    select 1 from dorf.sandbox_branches b
    join dorf.sessions s on s.id=b.destination_session_id
    join dorf.sandbox_delivery_holds h on h.id=b.id
    where b.destination_sandbox_id=sqlc.arg(sandbox_id)
      and b.restored_at is not null and b.release_requested_at is null
      and b.ready_at is null and h.released_at is null
      and s.admission_open and s.cleanup_state='pending'
)::boolean;
