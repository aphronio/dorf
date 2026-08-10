-- name: InsertEvidence :exec
insert into dorf.evidence(
    id,job_id,digest,byte_size,media_type,producer,kind,
    action_id,check_id,agent_run_id,revision,started_at,finished_at
)
values(
    sqlc.arg(id),sqlc.arg(job_id),sqlc.arg(digest),sqlc.arg(byte_size),
    sqlc.arg(media_type),sqlc.arg(producer),sqlc.arg(kind),
    nullif(sqlc.arg(action_id)::text,''),nullif(sqlc.arg(check_id)::text,''),
    nullif(sqlc.arg(agent_run_id)::text,''),
    nullif(sqlc.arg(revision)::text,''),sqlc.narg(started_at),sqlc.narg(finished_at)
)
on conflict(id) do nothing;

-- name: GetEvidenceIdentity :one
select job_id,digest,byte_size,media_type,producer,kind,
       coalesce(action_id,'') as action_id,coalesce(check_id,'') as check_id,
       coalesce(agent_run_id,'') as agent_run_id,
       coalesce(revision,'') as revision,started_at,finished_at
from dorf.evidence
where id=sqlc.arg(id);

-- name: InsertRepositoryCommand :exec
insert into dorf.repository_commands(job_id,name,command)
values(sqlc.arg(job_id),sqlc.arg(name),sqlc.arg(command))
on conflict do nothing;

-- name: GetRepositoryCommand :one
select command
from dorf.repository_commands
where job_id=sqlc.arg(job_id) and name=sqlc.arg(name);

-- name: ListDeclaredChecks :many
select name,command
from dorf.repository_commands
where job_id=sqlc.arg(job_id) and name in ('check','smoke')
order by case name when 'check' then 1 else 2 end;

-- name: InsertCheck :exec
insert into dorf.checks(id,job_id,name,command,revision,state)
values(sqlc.arg(id),sqlc.arg(job_id),sqlc.arg(name),sqlc.arg(command),sqlc.arg(revision),'running')
on conflict do nothing;

-- name: GetCheck :one
select id,job_id,name,command,revision,state,coalesce(exit_code,0)::integer as exit_code,
       coalesce(evidence_id,'') as evidence_id,started_at,finished_at
from dorf.checks
where id=sqlc.arg(id);

-- name: GetEvidenceDigest :one
select digest
from dorf.evidence
where id=sqlc.arg(id);

-- name: GetCheckForUpdate :one
select job_id,revision,command
from dorf.checks
where id=sqlc.arg(id)
for update;

-- name: CompleteCheck :exec
update dorf.checks
set state=sqlc.arg(state),exit_code=sqlc.arg(exit_code),
    evidence_id=sqlc.arg(evidence_id),started_at=sqlc.arg(started_at),
    finished_at=sqlc.arg(finished_at)
where id=sqlc.arg(id);

-- name: ListChecks :many
select c.id,c.job_id,c.name,c.command,c.revision,c.state,
       coalesce(c.exit_code,0)::integer as exit_code,
       coalesce(c.evidence_id,'') as evidence_id,
       coalesce(e.digest,'') as evidence_digest,
       c.started_at,c.finished_at
from dorf.checks c
left join dorf.evidence e on e.id=c.evidence_id
where c.job_id=sqlc.arg(job_id)
order by c.started_at nulls last,c.id;

-- name: ListEvidence :many
select id,digest,byte_size,media_type,producer,kind,
       coalesce(action_id,'') as action_id,coalesce(check_id,'') as check_id,
       coalesce(agent_run_id,'') as agent_run_id,
       coalesce(revision,'') as revision,started_at,finished_at
from dorf.evidence
where job_id=sqlc.arg(job_id)
order by created_at,id;
