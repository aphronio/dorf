-- name: GetJob :one
select j.id,j.admission_key,j.workflow_name,j.workflow_revision,j.goal,
       j.sandbox_profile,j.provider_connection,j.model,j.reasoning_effort,j.admission_open,
       j.cleanup_state,coalesce(current_task.task_id,'') as current_task_id,
       coalesce(j.workflow_attention,'') as workflow_attention,
       coalesce(j.workflow_attention_source,'') as workflow_attention_source,
       j.workflow_attention_at,coalesce(j.cleanup_attention,'') as cleanup_attention,
       j.admitted_at,j.cleaned_at
from dorf.jobs j
left join lateral (
    select task_id from dorf.job_tasks where job_id=j.id order by sequence desc limit 1
) current_task on true
where j.id=sqlc.arg(job_id);

-- name: GetCodingJob :one
select j.id,j.admission_key,j.workflow_name,j.workflow_revision,j.goal,
       c.repository,c.starting_revision,c.revision,c.branch,
       c.github_repository,c.github_installation_id,c.base_branch,
       j.sandbox_profile,j.provider_connection,j.model,j.reasoning_effort,j.admission_open,
       j.cleanup_state,coalesce(current_task.task_id,'') as current_task_id,
       coalesce(j.workflow_attention,'') as workflow_attention,
       coalesce(j.workflow_attention_source,'') as workflow_attention_source,
       j.workflow_attention_at,coalesce(j.cleanup_attention,'') as cleanup_attention,
       j.admitted_at,j.cleaned_at
from dorf.jobs j
join dorf.coding_to_proposal_inputs c on c.job_id=j.id
left join lateral (
    select task_id from dorf.job_tasks where job_id=j.id order by sequence desc limit 1
) current_task on true
where j.id=sqlc.arg(job_id);

-- name: GetRevisionJobForUpdate :one
select c.revision,c.branch,j.admission_open,
       exists(select 1 from dorf.job_outcomes where job_id=j.id) as outcome_exists
from dorf.jobs j
join dorf.coding_to_proposal_inputs c on c.job_id=j.id
where j.id=sqlc.arg(job_id)
for update of j,c;

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
update dorf.coding_to_proposal_inputs
set revision=sqlc.arg(revision)
where job_id=sqlc.arg(job_id) and revision=sqlc.arg(comparison_base_oid);

-- name: InsertAdmittedJob :execrows
insert into dorf.jobs(
    id,admission_key,workflow_name,workflow_revision,goal,
    sandbox_profile,provider_connection,model,reasoning_effort
)
values(
    sqlc.arg(id),sqlc.arg(admission_key),sqlc.arg(workflow_name),sqlc.arg(workflow_revision),
    sqlc.arg(goal),
    sqlc.arg(sandbox_profile),sqlc.arg(provider_connection),sqlc.arg(model),
    sqlc.arg(reasoning_effort)
)
on conflict(admission_key) do nothing;

-- name: GetAdmittedJobForUpdate :one
select id,admission_key,workflow_name,workflow_revision,goal,sandbox_profile,provider_connection,
       model,reasoning_effort
from dorf.jobs
where admission_key=sqlc.arg(admission_key)
for update;

-- name: InsertCodingToProposalInput :execrows
insert into dorf.coding_to_proposal_inputs(
    job_id,workflow_name,repository,starting_revision,revision,branch,
    github_repository,github_installation_id,base_branch
) values(
    sqlc.arg(job_id),'coding-to-proposal',sqlc.arg(repository),sqlc.arg(starting_revision),sqlc.arg(revision),
    sqlc.arg(branch),sqlc.arg(github_repository),sqlc.arg(github_installation_id),sqlc.arg(base_branch)
)
on conflict(job_id) do nothing;

-- name: GetCodingToProposalInput :one
select job_id,repository,starting_revision,revision,branch,
       github_repository,github_installation_id,base_branch
from dorf.coding_to_proposal_inputs
where job_id=sqlc.arg(job_id);

-- name: InsertInitialRevision :exec
insert into dorf.revisions(job_id,oid,branch,generation)
values(sqlc.arg(job_id),sqlc.arg(oid),sqlc.arg(branch),0)
on conflict do nothing;

-- name: GetJobAdmissionForUpdate :one
select workflow_name,workflow_revision,admission_open,
       exists(select 1 from dorf.job_outcomes where job_id=dorf.jobs.id) as outcome_exists
from dorf.jobs
where id=sqlc.arg(job_id)
for update;

-- name: GetJobSandboxProfileForUpdate :one
select sandbox_profile
from dorf.jobs
where id=sqlc.arg(job_id)
for update;

-- name: GetCurrentJobTaskForUpdate :one
select coalesce(current_task.task_id,'') as task_id,
       coalesce(current_task.task_name,'') as task_name,
       coalesce(current_task.sequence,0)::bigint as sequence
from dorf.jobs j
left join lateral (
    select task_id,task_name,sequence
    from dorf.job_tasks where job_id=j.id order by sequence desc limit 1
) current_task on true
where j.id=sqlc.arg(job_id)
for update of j;

-- name: ListJobTasks :many
select job_id,sequence,task_id,task_name,attached_at
from dorf.job_tasks
where job_id=sqlc.arg(job_id)
order by sequence;

-- name: InsertJobTask :execrows
insert into dorf.job_tasks(job_id,sequence,task_id,task_name)
values(sqlc.arg(job_id),sqlc.arg(sequence),sqlc.arg(task_id),sqlc.arg(task_name))
on conflict(task_id) do nothing;

-- name: MarkCleanupScheduled :execrows
update dorf.jobs
set cleanup_state=case when cleanup_state='complete' or cleaned_at is not null then 'complete' else 'scheduled' end
where id=sqlc.arg(job_id);

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
