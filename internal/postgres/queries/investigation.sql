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

-- name: ListCodebaseInvestigationDrafts :many
select d.job_id,d.agent_run_id,ar.message_id,d.artifact_id,a.created_at
from dorf.codebase_investigation_drafts d
join dorf.artifacts a on a.job_id=d.job_id and a.id=d.artifact_id
join dorf.agent_runs ar on ar.job_id=d.job_id and ar.id=d.agent_run_id
join dorf.job_messages m on m.id=ar.message_id
where d.job_id=sqlc.arg(job_id)
order by m.sequence,d.artifact_id;

-- name: ListCodebaseInvestigationMessages :many
select m.id as message_id,ar.sandbox_id
from dorf.job_messages m
join dorf.agent_runs ar on ar.message_id=m.id
where m.job_id=sqlc.arg(job_id) and ar.role='investigate'
order by m.sequence;

-- name: GetCodebaseInvestigationRunForUpdate :one
select coalesce(j.workflow_name,'') as workflow_name,
       coalesce(j.workflow_revision,'') as workflow_revision,
       s.revision,j.admission_open,j.cleanup_state,
       ar.id as agent_run_id,ar.role,ar.state,coalesce(ar.turn_id,'') as turn_id,
       coalesce(ar.turn_outcome,'') as turn_outcome,coalesce(ar.input_revision,'') as input_revision,
       ar.started_at,ar.finished_at
from dorf.jobs j
join dorf.codebase_investigation_sources s on s.job_id=j.id
join dorf.agent_runs ar on ar.job_id=j.id
where j.id=sqlc.arg(job_id) and ar.id=sqlc.arg(agent_run_id)
for update of j,ar;

-- name: InsertCodebaseInvestigationDraft :execrows
insert into dorf.codebase_investigation_drafts(
    job_id,agent_run_id,artifact_id
) values(
    sqlc.arg(job_id),sqlc.arg(agent_run_id),sqlc.arg(artifact_id)
)
on conflict(job_id,agent_run_id) do nothing;

-- name: GetLatestInvestigationRunAndDraft :one
select ar.id as agent_run_id,coalesce(ar.harness,'') as harness,
       coalesce(ar.thread_id,'') as thread_id,ar.state,
       coalesce(d.artifact_id,'') as artifact_id
from dorf.agent_runs ar
join dorf.job_messages m on m.id=ar.message_id
left join dorf.codebase_investigation_drafts d
  on d.job_id=ar.job_id and d.agent_run_id=ar.id
where ar.job_id=sqlc.arg(job_id) and ar.role='investigate'
order by m.sequence desc
limit 1;
