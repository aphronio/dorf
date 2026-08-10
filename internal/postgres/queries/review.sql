-- name: GetReviewJobForUpdate :one
select revision,admission_open,
       exists(select 1 from dorf.job_outcomes where job_id=dorf.jobs.id) as outcome_exists
from dorf.jobs
where id=sqlc.arg(job_id)
for update;

-- name: ListReviewCheckInputs :many
select r.name,coalesce(e.id,'') as evidence_id
from dorf.repository_commands r
left join dorf.checks c
  on c.job_id=r.job_id and c.name=r.name and c.command=r.command
 and c.revision=sqlc.arg(revision)
 and c.state='passed' and c.exit_code=0
left join dorf.evidence e
  on e.id=c.evidence_id and e.job_id=c.job_id and e.check_id=c.id and e.revision=c.revision
where r.job_id=sqlc.arg(job_id) and r.name in ('check','smoke')
order by r.name;

-- name: GetReviewPlan :one
select job_id,revision,facts::text as facts,plan::text as plan,
       policy_digest,created_at
from dorf.review_plans
where job_id=sqlc.arg(job_id) and revision=sqlc.arg(revision);

-- name: ListReviewPlans :many
select job_id,revision,facts::text as facts,plan::text as plan,
       policy_digest,created_at
from dorf.review_plans
where job_id=sqlc.arg(job_id)
order by created_at;

-- name: GetReviewPlanDigestForUpdate :one
select policy_digest
from dorf.review_plans
where job_id=sqlc.arg(job_id) and revision=sqlc.arg(revision)
for update;

-- name: InsertReviewPlan :execrows
insert into dorf.review_plans(job_id,revision,facts,plan,policy_digest)
values(sqlc.arg(job_id),sqlc.arg(revision),sqlc.arg(facts)::jsonb,
       sqlc.arg(plan)::jsonb,sqlc.arg(policy_digest)::text)
on conflict do nothing;

-- name: InsertReviewAgentRun :execrows
insert into dorf.agent_runs(id,job_id,message_id,role,state,input_revision,capability,sandbox_id,submission_nonce)
values(sqlc.arg(id),sqlc.arg(job_id),sqlc.arg(message_id),sqlc.arg(role),'pending',
       sqlc.arg(input_revision)::text,sqlc.arg(capability)::text,sqlc.arg(sandbox_id),sqlc.arg(submission_nonce)::text)
on conflict do nothing;

-- name: GetReviewRun :one
select *
from dorf.review_run_projection
where id=sqlc.arg(run_id) and role<>'implement';

-- name: ListAllReviewRuns :many
select sqlc.embed(p)
from dorf.review_run_projection p
where p.job_id=sqlc.arg(job_id) and p.input_revision<>'' and p.role<>'implement'
order by p.input_revision,p.role;

-- name: InterruptAgentRun :execrows
update dorf.agent_runs
set state='interrupted',
    turn_outcome=case when turn_id is null then null else 'interrupted' end,
    attention=sqlc.arg(reason)::text,
    finished_at=case when started_at is null then null else coalesce(finished_at,clock_timestamp()) end
where id=sqlc.arg(run_id) and state in ('pending','submitting','active','uncertain');

-- name: GetReviewFeedbackRunForUpdate :one
select ar.job_id,coalesce(ar.input_revision,'') as input_revision,ar.role,ar.state,coalesce(ar.turn_id,'') as turn_id,
       coalesce(ar.capability,'') as capability,j.revision as current_revision,j.admission_open,
       exists(select 1 from dorf.job_outcomes o where o.job_id=j.id)::boolean as outcome_exists,
       (ar.harness is not null and ar.thread_id is not null and ar.turn_id is not null
        and exists(select 1 from dorf.actions a where a.job_id=ar.job_id
            and a.kind='sandbox-create' and a.scope_key=ar.sandbox_id and a.state='succeeded')
        and exists(select 1 from dorf.actions a where a.job_id=ar.job_id
            and a.kind='provider-route-create' and a.scope_key=ar.sandbox_id and a.state='succeeded')
        and exists(select 1 from dorf.actions a where a.job_id=ar.job_id
            and a.kind='review-checkout' and a.scope_key=ar.sandbox_id and a.state='succeeded'))::boolean as boundary_ready
from dorf.agent_runs ar
join dorf.jobs j on j.id=ar.job_id
where ar.id=sqlc.arg(run_id)
for update of j,ar;
