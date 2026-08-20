-- name: InsertEvidence :exec
insert into dorf.evidence(
    id,job_id,digest,byte_size,media_type,producer,kind,
    action_id,agent_run_id,revision,started_at,finished_at
)
values(
    sqlc.arg(id),sqlc.arg(job_id),sqlc.arg(digest),sqlc.arg(byte_size),
    sqlc.arg(media_type),sqlc.arg(producer),sqlc.arg(kind),
    nullif(sqlc.arg(action_id)::text,''),nullif(sqlc.arg(agent_run_id)::text,''),
    nullif(sqlc.arg(revision)::text,''),sqlc.narg(started_at),sqlc.narg(finished_at)
)
on conflict(id) do nothing;

-- name: GetEvidenceIdentity :one
select job_id,digest,byte_size,media_type,producer,kind,
       coalesce(action_id,'') as action_id,coalesce(agent_run_id,'') as agent_run_id,
       coalesce(revision,'') as revision,started_at,finished_at
from dorf.evidence
where id=sqlc.arg(id);

-- name: ListEvidence :many
select id,digest,byte_size,media_type,producer,kind,
       coalesce(action_id,'') as action_id,coalesce(agent_run_id,'') as agent_run_id,
       coalesce(revision,'') as revision,started_at,finished_at
from dorf.evidence
where job_id=sqlc.arg(job_id)
order by created_at,id;
