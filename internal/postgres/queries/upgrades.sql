-- name: GetSandboxUpgrade :one
select u.*,s.session_id,coalesce(src.provider_id,'') as source_provider_id,coalesce(dst.provider_id,'') as destination_provider_id from dorf.sandbox_upgrades u join dorf.sandboxes s on s.id=u.sandbox_id join dorf.sandbox_resources src on src.id=u.source_resource_id left join dorf.sandbox_resources dst on dst.id=u.destination_resource_id where u.id=sqlc.arg(id);

-- name: ListSessionUpgrades :many
select u.*,s.session_id,coalesce(src.provider_id,'') as source_provider_id,coalesce(dst.provider_id,'') as destination_provider_id from dorf.sandbox_upgrades u join dorf.sandboxes s on s.id=u.sandbox_id join dorf.sandbox_resources src on src.id=u.source_resource_id left join dorf.sandbox_resources dst on dst.id=u.destination_resource_id
where s.session_id=sqlc.arg(session_id) order by u.requested_at,u.id;

-- name: InsertSandboxUpgrade :exec
insert into dorf.sandbox_upgrades(id,sandbox_id,source_resource_id,package_path,version)
values(sqlc.arg(id),sqlc.arg(sandbox_id),sqlc.arg(source_resource_id),sqlc.arg(package_path),sqlc.arg(version));

-- name: UpgradeQuiescent :one
select (j.native_pending_input_id is null and j.native_pending_turn_id is null)::boolean as quiet
from dorf.sessions j join dorf.sandboxes s on s.session_id=j.id
where s.id=sqlc.arg(sandbox_id);

-- name: RecordUpgradePreparation :execrows
update dorf.sandbox_upgrades set previous_version=sqlc.arg(previous_version)::text
where id=sqlc.arg(id) and (previous_version is null or previous_version=sqlc.arg(previous_version)::text);

-- name: RecordUpgradeQuiesced :execrows
update dorf.sandbox_upgrades set quiesced_at=coalesce(quiesced_at,clock_timestamp())
where id=sqlc.arg(id) and previous_version is not null;

-- name: RecordUpgradeCheckpoint :execrows
update dorf.sandbox_upgrades set checkpoint_key=sqlc.arg(checkpoint_key)::text,
checkpoint_reference=sqlc.arg(checkpoint_reference)::text,checkpoint_source_id=sqlc.arg(checkpoint_source_id)::text
where id=sqlc.arg(id) and quiesced_at is not null and
(checkpoint_reference is null or (checkpoint_key=sqlc.arg(checkpoint_key)::text
and checkpoint_reference=sqlc.arg(checkpoint_reference)::text and checkpoint_source_id=sqlc.arg(checkpoint_source_id)::text));

-- name: RecordUpgradeActivated :execrows
update dorf.sandbox_upgrades set activated_at=coalesce(activated_at,clock_timestamp())
where id=sqlc.arg(id) and checkpoint_reference is not null and rollback_at is null;

-- name: RequestUpgradeRollback :execrows
update dorf.sandbox_upgrades set rollback_at=coalesce(rollback_at,clock_timestamp()),failure_code=coalesce(failure_code,sqlc.arg(failure_code)::text)
where id=sqlc.arg(id) and checkpoint_reference is not null and verified_at is null;

-- name: BindUpgradeDestination :execrows
update dorf.sandbox_upgrades set destination_resource_id=sqlc.arg(resource_id)::text
where id=sqlc.arg(id) and rollback_at is not null and
(destination_resource_id is null or destination_resource_id=sqlc.arg(resource_id)::text);

-- name: RecordUpgradeRestored :execrows
update dorf.sandbox_upgrades set restored_at=coalesce(restored_at,clock_timestamp())
where id=sqlc.arg(id) and destination_resource_id is not null and rollback_at is not null;

-- name: RecordUpgradeVerified :execrows
update dorf.sandbox_upgrades set verified_at=coalesce(verified_at,clock_timestamp())
where id=sqlc.arg(id) and (activated_at is not null or restored_at is not null)
and (rollback_at is null or restored_at is not null);

-- name: RecordUpgradeCheckpointDeleted :execrows
update dorf.sandbox_upgrades set checkpoint_deleted_at=coalesce(checkpoint_deleted_at,clock_timestamp())
where id=sqlc.arg(id) and checkpoint_reference is not null;

-- name: RecordUpgradeFinished :execrows
update dorf.sandbox_upgrades set finished_at=coalesce(finished_at,clock_timestamp())
where id=sqlc.arg(id) and verified_at is not null;
