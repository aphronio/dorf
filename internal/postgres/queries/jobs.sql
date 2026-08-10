-- name: GetJob :one
select j.id,j.admission_key,j.goal,j.repository,j.revision,
       initial.oid as starting_revision,j.branch,
       coalesce(j.github_repository,'') as github_repository,coalesce(j.github_installation_id,'') as github_installation_id,
       coalesce(j.base_branch,'') as base_branch,
       j.provider_connection,j.model,j.reasoning_effort,j.admission_open,
       j.cleanup_state,coalesce(j.task_id,'') as task_id,coalesce(j.cleanup_task_id,'') as cleanup_task_id,
       coalesce(j.workflow_attention,'') as workflow_attention,
       coalesce(j.workflow_attention_source,'') as workflow_attention_source,
       j.workflow_attention_at,coalesce(j.cleanup_attention,'') as cleanup_attention,
       j.admitted_at,j.cleaned_at
from dorf.jobs j
join dorf.revisions initial on initial.job_id=j.id and initial.generation=0
where j.id=sqlc.arg(job_id);

-- name: GetRevisionJobForUpdate :one
select revision,branch,admission_open,
       exists(select 1 from dorf.job_outcomes where job_id=dorf.jobs.id) as outcome_exists
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

-- name: ListRevisions :many
select job_id,oid,coalesce(comparison_base_oid,'') as comparison_base_oid,
       coalesce(tree_oid,'') as tree_oid,branch,generation,
       coalesce(evidence_id,'') as evidence_id,observed_at
from dorf.revisions
where job_id=sqlc.arg(job_id)
order by generation;

-- name: AdvanceJobRevision :execrows
update dorf.jobs
set revision=sqlc.arg(revision)
where id=sqlc.arg(job_id) and revision=sqlc.arg(comparison_base_oid);

-- name: InsertAdmittedJob :execrows
insert into dorf.jobs(
    id,admission_key,goal,repository,revision,branch,
    provider_connection,model,reasoning_effort,
    github_repository,github_installation_id,base_branch
)
values(
    sqlc.arg(id),sqlc.arg(admission_key),sqlc.arg(goal),sqlc.arg(repository),
    sqlc.arg(revision),sqlc.arg(branch),
    sqlc.arg(provider_connection),sqlc.arg(model),
    sqlc.arg(reasoning_effort),sqlc.arg(github_repository),
    sqlc.arg(github_installation_id),sqlc.arg(base_branch)
)
on conflict(admission_key) do nothing;

-- name: GetAdmittedJobForUpdate :one
select id,admission_key,goal,repository,revision,branch,provider_connection,
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
select admission_open,
       exists(select 1 from dorf.job_outcomes where job_id=dorf.jobs.id) as outcome_exists
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
set admission_open=false
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

-- name: GetSelectedSetupAction :one
select a.id,a.job_id,a.kind,a.state,a.scope_key,a.created_at,a.settled_at
from dorf.jobs j
join dorf.actions a on a.id=j.setup_action_id and a.job_id=j.id
where j.id=sqlc.arg(job_id);

-- name: SelectInitialSetupAction :execrows
update dorf.jobs
set setup_action_id=sqlc.arg(action_id)
where id=sqlc.arg(job_id) and setup_action_id is null;

-- name: GetSetupRetryJobForUpdate :one
select coalesce(setup_action_id,'') as setup_action_id,admission_open,
       exists(select 1 from dorf.job_outcomes where job_id=dorf.jobs.id) as outcome_exists
from dorf.jobs
where id=sqlc.arg(job_id)
for update;

-- name: SelectSetupRetry :execrows
update dorf.jobs
set setup_action_id=sqlc.arg(action_id),
    workflow_attention=null,workflow_attention_source=null,workflow_attention_at=null
where dorf.jobs.id=sqlc.arg(job_id) and setup_action_id=sqlc.arg(previous_action_id)
  and (workflow_attention_source is null or workflow_attention_source=sqlc.arg(previous_action_id))
  and exists (
      select 1 from dorf.actions a
      where a.id=sqlc.arg(previous_action_id) and a.job_id=dorf.jobs.id
        and a.kind='repository-setup' and a.state='failed'
  );

-- name: SetWorkflowAttention :execrows
update dorf.jobs
set workflow_attention=sqlc.arg(detail),
    workflow_attention_source=sqlc.arg(source),
    workflow_attention_at=clock_timestamp()
where id=sqlc.arg(job_id)
  and (workflow_attention_source is null or workflow_attention_source=sqlc.arg(source));

-- name: ClearWorkflowAttention :execrows
update dorf.jobs
set workflow_attention=null,workflow_attention_source=null,workflow_attention_at=null
where id=sqlc.arg(job_id) and workflow_attention_source=sqlc.arg(source);

-- name: SetCleanupAttention :execrows
update dorf.jobs
set cleanup_attention=nullif(sqlc.arg(detail)::text,'')
where id=sqlc.arg(job_id) and cleanup_state<>'complete';
