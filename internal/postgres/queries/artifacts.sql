-- name: InsertArtifact :exec
insert into dorf.artifacts(
    id,job_id,name,digest,byte_size,media_type,producer,agent_run_id,created_at
) values(
    sqlc.arg(id),sqlc.arg(job_id),sqlc.arg(name),sqlc.arg(digest),
    sqlc.arg(byte_size),sqlc.arg(media_type),sqlc.arg(producer),
    sqlc.arg(agent_run_id),sqlc.arg(created_at)
)
on conflict(id) do nothing;

-- name: GetArtifact :one
select id,job_id,name,digest,byte_size,media_type,producer,agent_run_id,created_at
from dorf.artifacts
where id=sqlc.arg(id);

-- name: ListArtifacts :many
select id,job_id,name,digest,byte_size,media_type,producer,agent_run_id,created_at
from dorf.artifacts
where job_id=sqlc.arg(job_id)
order by name,id;
