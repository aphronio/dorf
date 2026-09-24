-- name: GetCheckpointBoundary :one
select
    j.id as session_id,
    s.id as sandbox_id,
    s.active_resource_id as resource_id,
    j.sandbox_profile as profile_name,
    j.sandbox_profile_revision as profile_revision,
    coalesce((
        select u.id
        from dorf.sandbox_upgrades u
        where u.sandbox_id=s.id and u.finished_at is not null and u.rollback_at is null
        order by u.finished_at desc,u.id desc
        limit 1
    ),'')::text as effective_upgrade_id,
    coalesce(j.sandbox_last_active_at,j.admitted_at) as last_activity_at,
    j.native_revision,
    (select count(*) from dorf.sandbox_delivery_holds h where h.sandbox_id=s.id)::bigint
        as delivery_hold_count,
    (
        (j.sandbox_last_active_at is not null or sqlc.arg(cleanup)::boolean)
        and r.provider_id is not null
        and r.deleted_at is null
        and j.native_pending_input_id is null and j.native_pending_turn_id is null
        and not exists (
            select 1 from dorf.sandbox_delivery_holds h
            where h.sandbox_id=s.id and h.released_at is null
        )
        and (
            (not sqlc.arg(cleanup)::boolean and j.admission_open and j.cleanup_state='pending')
            or
            (sqlc.arg(cleanup)::boolean and not j.admission_open and j.cleanup_state in ('requested','scheduled'))
        )
    )::boolean as eligible
from dorf.sandboxes s
join dorf.sessions j on j.id=s.session_id
join dorf.sandbox_resources r on r.sandbox_id=s.id and r.id=s.active_resource_id
where s.id=sqlc.arg(sandbox_id);

-- name: InsertSandboxCheckpoint :execrows
insert into dorf.sandbox_checkpoints(
    id,repository,snapshot_id,sandbox_id,resource_id,profile_name,profile_revision,
    effective_upgrade_id,last_activity_at,native_revision,
    delivery_hold_count,cleanup
)
values(
    coalesce(nullif(sqlc.arg(id)::text,''),gen_random_uuid()::text),sqlc.arg(repository),sqlc.arg(snapshot_id),sqlc.arg(sandbox_id),sqlc.arg(resource_id),
    sqlc.arg(profile_name),sqlc.arg(profile_revision),nullif(sqlc.arg(effective_upgrade_id)::text,''),
    sqlc.arg(last_activity_at),sqlc.arg(native_revision),
    sqlc.arg(delivery_hold_count),sqlc.arg(cleanup)
)
on conflict do nothing;

-- name: GetSandboxCheckpointByReference :one
select c.*,s.session_id
from dorf.sandbox_checkpoints c
join dorf.sandboxes s on s.id=c.sandbox_id
where c.repository=sqlc.arg(repository) and c.snapshot_id=sqlc.arg(snapshot_id);

-- name: GetLastSandboxCheckpoint :one
select c.*,s.session_id
from dorf.sandbox_checkpoints c
join dorf.sandboxes s on s.id=c.sandbox_id
where c.sandbox_id=sqlc.arg(sandbox_id)
order by c.native_revision desc,c.publication_sequence desc
limit 1;

-- name: ListSandboxCheckpoints :many
select c.*,s.session_id
from dorf.sandbox_checkpoints c
join dorf.sandboxes s on s.id=c.sandbox_id
where c.sandbox_id=sqlc.arg(sandbox_id)
order by c.native_revision desc,c.publication_sequence desc;

-- name: ListIdleCheckpointSandboxIDs :many
select s.id
from dorf.sandboxes s
join dorf.sessions j on j.id=s.session_id
join dorf.sandbox_resources r on r.sandbox_id=s.id and r.id=s.active_resource_id
where j.admission_open and j.cleanup_state='pending'
  and j.sandbox_last_active_at is not null
  and j.sandbox_last_active_at <= clock_timestamp()-make_interval(secs => sqlc.arg(seconds)::double precision)
  and r.provider_id is not null and r.deleted_at is null
  and j.native_revision>0 and j.native_pending_input_id is null and j.native_pending_turn_id is null
  and not exists (
      select 1 from dorf.sandbox_delivery_holds h
      where h.sandbox_id=s.id and h.released_at is null
  )
  and not coalesce((
      select
          c.resource_id=s.active_resource_id
          and c.profile_name=j.sandbox_profile
          and c.profile_revision=j.sandbox_profile_revision
          and c.effective_upgrade_id is not distinct from (
              select u.id
              from dorf.sandbox_upgrades u
              where u.sandbox_id=s.id and u.finished_at is not null and u.rollback_at is null
              order by u.finished_at desc,u.id desc
              limit 1
          )
          and c.last_activity_at=j.sandbox_last_active_at
          and c.native_revision=j.native_revision
          and c.delivery_hold_count=(
              select count(*) from dorf.sandbox_delivery_holds h where h.sandbox_id=s.id
          )
          and not c.cleanup
      from dorf.sandbox_checkpoints c
      where c.sandbox_id=s.id
      order by c.native_revision desc,c.publication_sequence desc
      limit 1
  ),false)
order by j.sandbox_last_active_at,s.id
limit 100;

-- name: GetSandboxCheckpointByID :one
select c.*,s.session_id from dorf.sandbox_checkpoints c
join dorf.sandboxes s on s.id=c.sandbox_id where c.id=sqlc.arg(id);
