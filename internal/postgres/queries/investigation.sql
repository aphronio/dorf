-- name: InsertCodebaseInvestigationSource :execrows
insert into dorf.codebase_investigation_sources(
    job_id,kind,bundle_digest,bundle_byte_size
) values(
    sqlc.arg(job_id),sqlc.arg(kind),sqlc.arg(bundle_digest),sqlc.arg(bundle_byte_size)
)
on conflict(job_id) do nothing;

-- name: GetCodebaseInvestigationSource :one
select s.job_id,s.kind,j.repository,j.revision,
       coalesce(s.bundle_digest,'') as bundle_digest,coalesce(s.bundle_byte_size,0) as bundle_byte_size
from dorf.codebase_investigation_sources s
join dorf.jobs j on j.id=s.job_id
where s.job_id=sqlc.arg(job_id);

-- name: GetCodebaseInvestigationReport :one
select r.job_id,r.report_artifact_id,a.created_at as observed_at
from dorf.codebase_investigation_reports r
join dorf.artifacts a on a.job_id=r.job_id and a.id=r.report_artifact_id
where r.job_id=sqlc.arg(job_id);

-- name: GetCodebaseInvestigationRunForUpdate :one
select j.workflow_name,j.workflow_revision,j.revision,j.admission_open,j.cleanup_state,
       ar.id as agent_run_id,ar.role,ar.state,coalesce(ar.turn_id,'') as turn_id,
       coalesce(ar.turn_outcome,'') as turn_outcome,coalesce(ar.input_revision,'') as input_revision,
       ar.started_at,ar.finished_at
from dorf.jobs j
join dorf.agent_runs ar on ar.job_id=j.id
where j.id=sqlc.arg(job_id) and ar.id=sqlc.arg(agent_run_id)
for update of j,ar;

-- name: InsertCodebaseInvestigationReport :execrows
insert into dorf.codebase_investigation_reports(
    job_id,report_artifact_id
) values(
    sqlc.arg(job_id),sqlc.arg(report_artifact_id)
);

-- name: CloseAdmissionForCodebaseInvestigation :execrows
update dorf.jobs
set admission_open=false
where id=sqlc.arg(job_id) and admission_open and cleanup_state='pending'
  and workflow_name='codebase-investigation' and workflow_revision='1';
