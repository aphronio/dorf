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
