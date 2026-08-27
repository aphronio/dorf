-- name: InsertCodebaseInvestigationSource :execrows
insert into dorf.codebase_investigation_sources(
    job_id,workflow_name,repository,revision
) values(
    sqlc.arg(job_id),'codebase-investigation',sqlc.arg(repository),sqlc.arg(revision)
)
on conflict(job_id) do nothing;

-- name: GetCodebaseInvestigationSource :one
select s.job_id,s.repository,s.revision
from dorf.codebase_investigation_sources s
where s.job_id=sqlc.arg(job_id);
