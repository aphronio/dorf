-- name: GetPublicationJobForUpdate :one
select revision,workflow_phase,coalesce(github_repository,'') as github_repository,
       coalesce(github_installation_id,'') as github_installation_id,
       coalesce(base_branch,'') as base_branch,branch,repository,
       admission_open,cleanup_state
from dorf.jobs
where id=sqlc.arg(job_id)
for update;

-- name: StartPublicationIntent :execrows
update dorf.jobs
set workflow_phase='publishing',workflow_attention=null
where id=sqlc.arg(job_id) and revision=sqlc.arg(revision) and workflow_phase='ready';

-- name: InsertPublicationAction :exec
insert into dorf.actions(id,job_id,kind,state,scope_key)
values(sqlc.arg(id),sqlc.arg(job_id),sqlc.arg(kind),'unsettled',sqlc.arg(scope_key))
on conflict do nothing;

-- name: GetPublicationActionForUpdate :one
select id,job_id,kind,state,scope_key
from dorf.actions
where id=sqlc.arg(id) and job_id=sqlc.arg(job_id) and kind=sqlc.arg(kind) and scope_key=sqlc.arg(scope_key)
for update;

-- name: GetPublicationAction :one
select id,job_id,kind,state,scope_key
from dorf.actions
where job_id=sqlc.arg(job_id) and kind=sqlc.arg(kind) and scope_key=sqlc.arg(scope_key);

-- name: ResumePublicationPhase :execrows
update dorf.jobs
set workflow_phase='publishing',workflow_attention=null
where id=sqlc.arg(job_id) and revision=sqlc.arg(revision)
  and workflow_phase='publication-blocked';

-- name: CompleteRepositoryPush :execrows
update dorf.actions
set state='succeeded'
where id=sqlc.arg(action_id) and kind='repository-push' and scope_key=sqlc.arg(revision);

-- name: GetProposalJobForUpdate :one
select revision,workflow_phase
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
set state='succeeded'
where id=sqlc.arg(action_id) and job_id=sqlc.arg(job_id)
  and kind='github-pull-request' and scope_key=sqlc.arg(proposed_revision);

-- name: CompletePublication :execrows
update dorf.jobs
set workflow_phase='published',workflow_attention=null
where id=sqlc.arg(job_id) and revision=sqlc.arg(revision) and workflow_phase='publishing';

-- name: BlockPublicationPhase :execrows
update dorf.jobs
set workflow_phase='publication-blocked',workflow_attention=sqlc.arg(reason)::text
where id=sqlc.arg(job_id) and revision=sqlc.arg(revision) and workflow_phase='publishing';

-- name: GetProposalCurrentRevision :one
select revision from dorf.jobs where id=sqlc.arg(job_id);
