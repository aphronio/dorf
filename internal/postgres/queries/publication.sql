-- name: GetPublicationJobForUpdate :one
select revision,coalesce(github_repository,'') as github_repository,
       coalesce(github_installation_id,'') as github_installation_id,
       coalesce(base_branch,'') as base_branch,branch,repository,
       admission_open,cleanup_state
from dorf.jobs
where id=sqlc.arg(job_id)
for update;

-- name: InsertPublicationAction :exec
insert into dorf.actions(id,job_id,kind,state,scope_key)
values(sqlc.arg(id),sqlc.arg(job_id),sqlc.arg(kind),'unsettled',sqlc.arg(scope_key))
on conflict do nothing;

-- name: GetPublicationActionForUpdate :one
select id,job_id,kind,state,scope_key,created_at,settled_at
from dorf.actions
where id=sqlc.arg(id) and job_id=sqlc.arg(job_id) and kind=sqlc.arg(kind) and scope_key=sqlc.arg(scope_key)
for update;

-- name: GetPublicationAction :one
select id,job_id,kind,state,scope_key,created_at,settled_at
from dorf.actions
where job_id=sqlc.arg(job_id) and kind=sqlc.arg(kind) and scope_key=sqlc.arg(scope_key);

-- name: CompleteRepositoryPush :execrows
update dorf.actions
set state='succeeded',settled_at=coalesce(settled_at,clock_timestamp())
where id=sqlc.arg(action_id) and kind='repository-push' and scope_key=sqlc.arg(revision)
  and state in ('unsettled','succeeded');

-- name: GetProposalJobForUpdate :one
select revision,admission_open,cleanup_state
from dorf.jobs
where id=sqlc.arg(job_id)
for update;

-- name: GetRepositoryPushState :one
select state
from dorf.actions
where job_id=sqlc.arg(job_id) and kind='repository-push' and scope_key=sqlc.arg(revision);

-- GetProposal is the one proposal projection shared by publication and outcome code.
-- name: GetProposal :one
select job_id,pr_number,pr_url,proposed_revision,body_digest
from dorf.github_proposals
where job_id=sqlc.arg(job_id);

-- name: UpsertProposal :execrows
insert into dorf.github_proposals(
    job_id,pr_number,pr_url,proposed_revision,body_digest
) values(
    sqlc.arg(job_id),sqlc.arg(pr_number),sqlc.arg(pr_url),sqlc.arg(proposed_revision),sqlc.arg(body_digest)
)
on conflict(job_id) do update set
  pr_url=excluded.pr_url,proposed_revision=excluded.proposed_revision,
  body_digest=excluded.body_digest
where dorf.github_proposals.pr_number=excluded.pr_number;

-- name: CompleteProposalAction :execrows
update dorf.actions
set state='succeeded',settled_at=coalesce(settled_at,clock_timestamp())
where id=sqlc.arg(action_id) and job_id=sqlc.arg(job_id)
  and kind='github-pull-request' and scope_key=sqlc.arg(proposed_revision)
  and state in ('unsettled','succeeded');

-- name: SetPublicationAttention :execrows
update dorf.jobs
set workflow_attention=sqlc.arg(reason)::text,
    workflow_attention_source=sqlc.arg(action_id)::text,
    workflow_attention_at=clock_timestamp()
where dorf.jobs.id=sqlc.arg(job_id) and dorf.jobs.revision=sqlc.arg(revision)
  and dorf.jobs.admission_open and dorf.jobs.cleanup_state='pending'
  and exists (
    select 1 from dorf.actions a
    where a.id=sqlc.arg(action_id) and a.job_id=dorf.jobs.id
      and a.scope_key=sqlc.arg(revision)
      and a.kind in ('repository-push','github-pull-request')
  );

-- name: ClearPublicationAttention :exec
update dorf.jobs
set workflow_attention=null,workflow_attention_source=null,workflow_attention_at=null
where id=sqlc.arg(job_id) and revision=sqlc.arg(revision)
  and workflow_attention_source=sqlc.arg(action_id);

-- name: ClearPublicationAttentionForAction :exec
update dorf.jobs j
set workflow_attention=null,workflow_attention_source=null,workflow_attention_at=null
where j.revision=sqlc.arg(revision)
  and j.workflow_attention_source=sqlc.arg(action_id)
  and exists (
    select 1 from dorf.actions a
    where a.id=sqlc.arg(action_id) and a.job_id=j.id
      and a.scope_key=sqlc.arg(revision)
  );

-- name: GetProposalCurrentRevision :one
select revision from dorf.jobs where id=sqlc.arg(job_id);
