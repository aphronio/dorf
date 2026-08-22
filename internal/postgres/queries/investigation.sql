-- name: InsertCodebaseInvestigationSource :execrows
insert into dorf.codebase_investigation_sources(
    job_id,workflow_name,kind,repository,revision,bundle_digest,bundle_byte_size
) values(
    sqlc.arg(job_id),'codebase-investigation',sqlc.arg(kind),sqlc.arg(repository),sqlc.arg(revision),
    sqlc.arg(bundle_digest),sqlc.arg(bundle_byte_size)
)
on conflict(job_id) do nothing;

-- name: GetCodebaseInvestigationSource :one
select s.job_id,s.kind,s.repository,s.revision,
       coalesce(s.bundle_digest,'') as bundle_digest,coalesce(s.bundle_byte_size,0) as bundle_byte_size
from dorf.codebase_investigation_sources s
where s.job_id=sqlc.arg(job_id);

-- name: ListCodebaseInvestigationMessages :many
select m.id as message_id,ar.sandbox_id,ar.state,
       coalesce(ar.turn_outcome,'') as turn_outcome,coalesce(ar.attention,'') as attention
from dorf.job_messages m
join dorf.agent_runs ar on ar.message_id=m.id
where m.job_id=sqlc.arg(job_id) and ar.role='investigate'
order by m.sequence;

-- name: GetLatestInvestigationRun :one
select ar.id as agent_run_id,coalesce(ar.harness,'') as harness,
       coalesce(ar.thread_id,'') as thread_id,ar.state
from dorf.agent_runs ar
join dorf.job_messages m on m.id=ar.message_id
where ar.job_id=sqlc.arg(job_id) and ar.role='investigate'
order by m.sequence desc
limit 1;
