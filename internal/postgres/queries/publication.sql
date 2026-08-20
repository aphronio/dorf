-- name: GetPublicationJobForUpdate :one
select c.revision,c.github_repository,c.github_installation_id,
       c.base_branch,c.branch,c.repository,j.admission_open,j.cleanup_state
from dorf.jobs j
join dorf.coding_to_proposal_inputs c on c.job_id=j.id
where j.id=sqlc.arg(job_id)
for update of j,c;

-- name: CompleteRepositoryPush :execrows
update dorf.actions
set state='succeeded',settled_at=coalesce(settled_at,clock_timestamp())
where id=sqlc.arg(action_id) and kind='repository-push' and scope_key=sqlc.arg(revision)
  and state in ('unsettled','succeeded');

-- name: GetProposalJobForUpdate :one
select c.revision,j.admission_open,j.cleanup_state
from dorf.jobs j
join dorf.coding_to_proposal_inputs c on c.job_id=j.id
where j.id=sqlc.arg(job_id)
for update of j,c;

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
where dorf.jobs.id=sqlc.arg(job_id)
  and exists (
    select 1 from dorf.coding_to_proposal_inputs c
    where c.job_id=dorf.jobs.id and c.revision=sqlc.arg(revision)
  )
  and dorf.jobs.admission_open and dorf.jobs.cleanup_state='pending'
  and exists (
    select 1 from dorf.actions a
    where a.id=sqlc.arg(action_id) and a.job_id=dorf.jobs.id
      and a.scope_key=sqlc.arg(revision)
      and a.kind in ('repository-push','github-pull-request')
      and a.state='unsettled'
  );

-- name: ClearPublicationAttention :exec
update dorf.jobs
set workflow_attention=null,workflow_attention_source=null,workflow_attention_at=null
where id=sqlc.arg(job_id)
  and exists (select 1 from dorf.coding_to_proposal_inputs c where c.job_id=dorf.jobs.id and c.revision=sqlc.arg(revision))
  and workflow_attention_source=sqlc.arg(action_id);

-- name: ClearPublicationAttentionForAction :exec
update dorf.jobs j
set workflow_attention=null,workflow_attention_source=null,workflow_attention_at=null
where exists (select 1 from dorf.coding_to_proposal_inputs c where c.job_id=j.id and c.revision=sqlc.arg(revision))
  and j.workflow_attention_source=sqlc.arg(action_id)
  and exists (
    select 1 from dorf.actions a
    where a.id=sqlc.arg(action_id) and a.job_id=j.id
      and a.scope_key=sqlc.arg(revision)
  );
