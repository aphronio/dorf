-- name: GetJob :one
select j.id,j.admission_key,j.goal,j.repository,j.revision,coalesce(rv.generation,0)::integer as revision_generation,j.starting_revision,j.branch,
       coalesce(j.github_repository,'') as github_repository,coalesce(j.github_installation_id,'') as github_installation_id,
       coalesce(j.base_branch,'') as base_branch,
       j.provider_connection,coalesce(j.provider_gateway_state,'') as provider_gateway_state,j.model,j.reasoning_effort,j.admission_open,
       j.cleanup_state,coalesce(j.task_id,'') as task_id,coalesce(j.cleanup_task_id,'') as cleanup_task_id,
       coalesce(sb.incus_name,'') as sandbox_id,coalesce(sb.state,'') as sandbox_state,
       coalesce(r.route_id,'') as route_id,coalesce(r.state,'') as route_state,coalesce(se.native_session_id,'') as session_id,
       j.workflow_phase,coalesce(j.workflow_attention,'') as workflow_attention,coalesce(j.cleanup_attention,'') as cleanup_attention
from dorf.jobs j
left join dorf.sandboxes sb on sb.job_id=j.id
left join dorf.routes r on r.job_id=j.id
left join dorf.sessions se on se.job_id=j.id
left join dorf.revisions rv on rv.job_id=j.id and rv.oid=j.revision
where j.id=sqlc.arg(job_id);

-- name: GetRevisionJobForUpdate :one
select revision,branch,workflow_phase
from dorf.jobs
where id=sqlc.arg(job_id)
for update;

-- name: NextRevisionGeneration :one
select (coalesce(max(generation),0)+1)::integer
from dorf.revisions
where job_id=sqlc.arg(job_id);

-- name: InsertRevision :exec
insert into dorf.revisions(job_id,oid,comparison_base_oid,tree_oid,branch,generation,evidence_id)
values(sqlc.arg(job_id),sqlc.arg(oid),sqlc.arg(comparison_base_oid)::text,sqlc.arg(tree_oid)::text,
       sqlc.arg(branch),sqlc.arg(generation),sqlc.arg(evidence_id)::text);

-- name: AdvanceJobRevision :execrows
update dorf.jobs
set revision=sqlc.arg(revision),workflow_phase='checking',workflow_attention=null
where id=sqlc.arg(job_id) and revision=sqlc.arg(comparison_base_oid);

-- name: InsertAdmittedJob :execrows
insert into dorf.jobs(
    id,admission_key,goal,repository,revision,starting_revision,branch,
    provider_connection,provider_gateway_state,model,reasoning_effort,
    github_repository,github_installation_id,base_branch
)
values(
    sqlc.arg(id),sqlc.arg(admission_key),sqlc.arg(goal),sqlc.arg(repository),
    sqlc.arg(revision),sqlc.arg(revision),sqlc.arg(branch),
    sqlc.arg(provider_connection),sqlc.arg(provider_gateway_state),sqlc.arg(model),
    sqlc.arg(reasoning_effort),sqlc.arg(github_repository),
    sqlc.arg(github_installation_id),sqlc.arg(base_branch)
)
on conflict(admission_key) do nothing;

-- name: GetAdmittedJobForUpdate :one
select id,admission_key,goal,repository,revision,branch,provider_connection,
       coalesce(provider_gateway_state,'') as provider_gateway_state,
       model,reasoning_effort,coalesce(github_repository,'') as github_repository,
       coalesce(github_installation_id,'') as github_installation_id,
       coalesce(base_branch,'') as base_branch
from dorf.jobs
where admission_key=sqlc.arg(admission_key)
for update;

-- name: InsertInitialRevision :exec
insert into dorf.revisions(job_id,oid,branch,generation)
values(sqlc.arg(job_id),sqlc.arg(oid),sqlc.arg(branch),0)
on conflict do nothing;

-- name: GetJobAdmissionForUpdate :one
select admission_open,workflow_phase
from dorf.jobs
where id=sqlc.arg(job_id)
for update;

-- name: AttachMessageTask :execrows
update dorf.jobs
set task_id=coalesce(task_id,sqlc.arg(task_id))
where id=sqlc.arg(job_id) and admission_open
  and (task_id is null or task_id=sqlc.arg(task_id));

-- name: CloseAdmission :execrows
update dorf.jobs
set admission_open=false,workflow_attention=null
where id=sqlc.arg(job_id);

-- name: SetCleanupTaskID :execrows
update dorf.jobs
set cleanup_task_id=coalesce(cleanup_task_id,sqlc.arg(cleanup_task_id)),
    cleanup_state=case when cleanup_state='complete' or cleaned_at is not null then 'complete' else 'scheduled' end
where id=sqlc.arg(job_id)
  and (cleanup_task_id is null or cleanup_task_id=sqlc.arg(cleanup_task_id));

-- name: GetSetupActionIDForUpdate :one
select coalesce(setup_action_id,'') as setup_action_id
from dorf.jobs
where id=sqlc.arg(job_id)
for update;

-- name: SelectInitialSetupAction :execrows
update dorf.jobs
set setup_action_id=sqlc.arg(action_id)
where id=sqlc.arg(job_id) and setup_action_id is null;

-- name: GetSetupRetryJobForUpdate :one
select workflow_phase,coalesce(setup_action_id,'') as setup_action_id,admission_open
from dorf.jobs
where id=sqlc.arg(job_id)
for update;

-- name: SelectSetupRetry :execrows
update dorf.jobs
set setup_action_id=sqlc.arg(action_id),workflow_phase='setup',workflow_attention=null
where id=sqlc.arg(job_id) and setup_action_id=sqlc.arg(previous_action_id)
  and workflow_phase='blocked';

-- name: GetRevisionPhaseForUpdate :one
select revision,workflow_phase
from dorf.jobs
where id=sqlc.arg(job_id)
for update;

-- name: CompleteUnchangedRun :execrows
update dorf.jobs
set workflow_phase=case when exists (
      select 1 from dorf.github_proposals p
      where p.job_id=dorf.jobs.id and p.proposed_revision=dorf.jobs.revision
        and p.observed_remote_head=dorf.jobs.revision
    ) then 'published' else 'blocked' end,
    workflow_attention=case when exists (
      select 1 from dorf.github_proposals p
      where p.job_id=dorf.jobs.id and p.proposed_revision=dorf.jobs.revision
        and p.observed_remote_head=dorf.jobs.revision
    ) then null else sqlc.arg(reason) end
where id=sqlc.arg(job_id) and revision=sqlc.arg(revision)
  and workflow_phase='implementing';

-- name: SetWorkflowPhaseAfterSetup :execrows
update dorf.jobs
set workflow_phase=sqlc.arg(workflow_phase),
    workflow_attention=nullif(sqlc.arg(workflow_attention)::text,'')
where id=sqlc.arg(job_id) and workflow_phase in ('setup','blocked');

-- name: ReturnFailedCheckToImplementation :execrows
update dorf.jobs
set workflow_phase='implementing',workflow_attention=null
where id=sqlc.arg(job_id) and revision=sqlc.arg(revision) and workflow_phase='checking';

-- name: MarkReady :execrows
update dorf.jobs
set workflow_phase='ready',workflow_attention=null
where id=sqlc.arg(job_id) and revision=sqlc.arg(revision) and workflow_phase='checking';

-- name: BlockWorkflow :execrows
update dorf.jobs
set workflow_phase='blocked',workflow_attention=sqlc.arg(reason)
where id=sqlc.arg(job_id);

-- name: GetWorkflowPhaseForUpdate :one
select workflow_phase
from dorf.jobs
where id=sqlc.arg(job_id)
for update;

-- name: BlockDelivery :exec
update dorf.jobs
set workflow_phase='blocked',workflow_attention=sqlc.arg(reason)
where id=sqlc.arg(job_id);

-- name: SetCleanupAttention :execrows
update dorf.jobs
set cleanup_attention=nullif(sqlc.arg(detail)::text,'')
where id=sqlc.arg(job_id) and cleanup_state<>'complete';
