-- name: GetCodebaseInvestigationReport :one
select job_id,agent_run_id,report_evidence_id,observed_at
from dorf.codebase_investigation_reports
where job_id=sqlc.arg(job_id);

-- name: GetCodebaseInvestigationRunForUpdate :one
select j.workflow_name,j.workflow_revision,j.revision,j.admission_open,j.cleanup_state,
       ar.id as agent_run_id,ar.role,ar.state,coalesce(ar.turn_id,'') as turn_id,
       coalesce(ar.turn_outcome,'') as turn_outcome,coalesce(ar.input_revision,'') as input_revision,
       ar.started_at,ar.finished_at
from dorf.jobs j
join dorf.agent_runs ar on ar.job_id=j.id
where j.id=sqlc.arg(job_id) and ar.id=sqlc.arg(agent_run_id)
for update of j,ar;

-- name: InsertCodebaseInvestigationReport :one
insert into dorf.codebase_investigation_reports(
    job_id,agent_run_id,report_evidence_id,observed_at
) values(
    sqlc.arg(job_id),sqlc.arg(agent_run_id),
    sqlc.arg(report_evidence_id),sqlc.arg(observed_at)
)
returning job_id,agent_run_id,report_evidence_id,observed_at;

-- name: CloseAdmissionForCodebaseInvestigation :execrows
update dorf.jobs
set admission_open=false
where id=sqlc.arg(job_id) and admission_open and cleanup_state='pending'
  and workflow_name='codebase-investigation' and workflow_revision='1';
