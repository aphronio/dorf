-- name: InsertControlEnrollment :one
with db_time as (select clock_timestamp() as now)
insert into dorf.control_enrollments(id,secret_digest,expires_at,created_at)
select sqlc.arg(id),sqlc.arg(secret_digest),now + sqlc.arg(lifetime_microseconds)::bigint * interval '1 microsecond',now
from db_time
returning expires_at;

-- name: GetControlEnrollmentForUpdate :one
select secret_digest,expires_at>clock_timestamp() as active,consumed_at,client_id
from dorf.control_enrollments
where id=sqlc.arg(id)
for update;

-- name: InsertControlClient :one
with db_time as (select clock_timestamp() as now)
insert into dorf.control_clients(id,name,credential_digest,credential_expires_at,created_at)
select sqlc.arg(id),sqlc.arg(name),sqlc.arg(credential_digest),now + sqlc.arg(lifetime_microseconds)::bigint * interval '1 microsecond',now
from db_time
returning id,name,credential_expires_at;

-- name: BindControlEnrollment :exec
update dorf.control_enrollments
set consumed_at=clock_timestamp(),client_id=sqlc.arg(client_id)
where id=sqlc.arg(id) and consumed_at is null;

-- name: AuthenticateControlClient :one
select id,name,credential_expires_at
from dorf.control_clients
where credential_digest=sqlc.arg(credential_digest)
  and revoked_at is null
  and credential_expires_at>clock_timestamp();

-- name: RevokeControlClient :execrows
update dorf.control_clients
set revoked_at=coalesce(revoked_at,clock_timestamp())
where id=sqlc.arg(id);
