-- name: GetCheckpointRecovery :one
select
    recovery.*,
    source.deleted_at as source_deleted_at,
    destination.deleted_at as destination_deleted_at,
    sandbox.session_id,
    coalesce(source.provider_id,'') as source_provider_id,
    coalesce(destination.provider_id,'') as destination_provider_id,
    checkpoint.resource_id as checkpoint_resource_id,
    checkpoint.profile_name,
    checkpoint.profile_revision,
    coalesce(checkpoint.effective_upgrade_id,'') as effective_upgrade_id,
    checkpoint.last_activity_at,
    checkpoint.message_sequence,
    checkpoint.completed_turn_sequence,
    checkpoint.delivery_hold_count,
    checkpoint.cleanup,
    checkpoint.published_at,
    coalesce(package.package_path,'') as package_path,
    coalesce(package.version,'') as package_version
from dorf.sandbox_recoveries recovery
join dorf.sandboxes sandbox on sandbox.id=recovery.sandbox_id
join dorf.sandbox_resources source on source.id=recovery.source_resource_id
left join dorf.sandbox_resources destination on destination.id=recovery.destination_resource_id
join dorf.sandbox_checkpoints checkpoint
  on checkpoint.repository=recovery.checkpoint_repository
 and checkpoint.snapshot_id=recovery.checkpoint_snapshot_id
left join dorf.sandbox_upgrades package on package.id=checkpoint.effective_upgrade_id
where recovery.id=sqlc.arg(id);

-- name: ListSessionCheckpointRecoveries :many
select
    recovery.*,
    source.deleted_at as source_deleted_at,
    destination.deleted_at as destination_deleted_at,
    sandbox.session_id,
    coalesce(source.provider_id,'') as source_provider_id,
    coalesce(destination.provider_id,'') as destination_provider_id,
    checkpoint.resource_id as checkpoint_resource_id,
    checkpoint.profile_name,
    checkpoint.profile_revision,
    coalesce(checkpoint.effective_upgrade_id,'') as effective_upgrade_id,
    checkpoint.last_activity_at,
    checkpoint.message_sequence,
    checkpoint.completed_turn_sequence,
    checkpoint.delivery_hold_count,
    checkpoint.cleanup,
    checkpoint.published_at,
    coalesce(package.package_path,'') as package_path,
    coalesce(package.version,'') as package_version
from dorf.sandbox_recoveries recovery
join dorf.sandboxes sandbox on sandbox.id=recovery.sandbox_id
join dorf.sandbox_resources source on source.id=recovery.source_resource_id
left join dorf.sandbox_resources destination on destination.id=recovery.destination_resource_id
join dorf.sandbox_checkpoints checkpoint
  on checkpoint.repository=recovery.checkpoint_repository
 and checkpoint.snapshot_id=recovery.checkpoint_snapshot_id
left join dorf.sandbox_upgrades package on package.id=checkpoint.effective_upgrade_id
where sandbox.session_id=sqlc.arg(session_id)
order by recovery.requested_at,recovery.id;

-- name: InsertCheckpointRecoveryHold :exec
insert into dorf.sandbox_delivery_holds(id,sandbox_id,reason)
values(sqlc.arg(id),sqlc.arg(sandbox_id),'checkpoint_recovery');

-- name: InsertCheckpointRecovery :exec
insert into dorf.sandbox_recoveries(
    id,sandbox_id,checkpoint_repository,checkpoint_snapshot_id,source_resource_id,destination_resource_id
)
values(
    sqlc.arg(id),sqlc.arg(sandbox_id),sqlc.arg(checkpoint_repository),
    sqlc.arg(checkpoint_snapshot_id),sqlc.arg(source_resource_id),sqlc.arg(destination_resource_id)
);

-- name: RecoveryNativeStateSafe :one
select not exists (
    select 1
    from dorf.agent_runs run
    join dorf.session_messages message on message.id=run.message_id
    where run.sandbox_id=sqlc.arg(sandbox_id)
      and message.sequence>sqlc.arg(message_sequence)
      and (
        run.state<>'pending'
        or run.harness is not null
        or run.thread_id is not null
        or run.baseline_turn_id is not null
        or run.turn_id is not null
        or run.started_at is not null
        or run.finished_at is not null
      )
)::boolean as safe;

-- name: RecordRecoveryVerified :execrows
update dorf.sandbox_recoveries set verified_at=coalesce(verified_at,clock_timestamp())
where id=sqlc.arg(id);

-- name: RecordCheckpointRecoveryFinished :execrows
update dorf.sandbox_recoveries set finished_at=coalesce(finished_at,clock_timestamp())
where id=sqlc.arg(id) and verified_at is not null;

-- name: AbandonCheckpointRecovery :execrows
update dorf.sandbox_delivery_holds hold
set released_at=coalesce(hold.released_at,clock_timestamp())
from dorf.sandbox_recoveries recovery
join dorf.sandboxes sandbox on sandbox.id=recovery.sandbox_id
join dorf.sessions session on session.id=sandbox.session_id
join dorf.sandbox_resources destination on destination.id=recovery.destination_resource_id
where recovery.id=sqlc.arg(id) and hold.id=recovery.id
  and hold.reason='checkpoint_recovery' and hold.sandbox_id=sandbox.id
  and recovery.finished_at is null and sandbox.active_resource_id=recovery.source_resource_id
  and destination.deleted_at is not null
  and not session.admission_open and session.cleanup_state='scheduled';
