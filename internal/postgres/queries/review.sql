-- name: GetReviewJobForUpdate :one
select revision, workflow_phase
from dorf.jobs
where id=sqlc.arg(job_id)
for update;

-- name: ListVerifiedReviewEvidenceIDs :many
select c.evidence_id
from dorf.repository_commands r
join dorf.checks c
  on c.job_id=r.job_id and c.name=r.name and c.command=r.command and c.revision=sqlc.arg(revision)
where r.job_id=sqlc.arg(job_id) and r.name in ('check','smoke') and c.state='passed' and c.exit_code=0
order by r.name;

-- name: CountDeclaredReviewChecks :one
select count(*)
from dorf.repository_commands
where job_id=sqlc.arg(job_id) and name in ('check','smoke');

-- name: InsertReviewPlan :exec
insert into dorf.review_plans(job_id,revision,state)
values(sqlc.arg(job_id),sqlc.arg(revision),'pending');

-- name: AdvanceJobToReviewPlanning :execrows
update dorf.jobs
set workflow_phase='review-planning', workflow_attention=null
where id=sqlc.arg(job_id) and revision=sqlc.arg(revision) and workflow_phase='checking';

-- name: GetReviewPlan :one
select job_id,revision,state,coalesce(facts,'{}'::jsonb)::text as facts,coalesce(plan,'{}'::jsonb)::text as plan,
       coalesce(policy_digest,'') as policy_digest,created_at,finalized_at
from dorf.review_plans
where job_id=sqlc.arg(job_id) and revision=sqlc.arg(revision);

-- name: ListReviewPlans :many
select job_id,revision,state,coalesce(facts,'{}'::jsonb)::text as facts,coalesce(plan,'{}'::jsonb)::text as plan,
       coalesce(policy_digest,'') as policy_digest,created_at,finalized_at
from dorf.review_plans
where job_id=sqlc.arg(job_id)
order by created_at;

-- name: GetReviewPlanForUpdate :one
select job_id,revision,state,coalesce(facts,'{}'::jsonb)::text as facts,coalesce(plan,'{}'::jsonb)::text as plan,
       coalesce(policy_digest,'') as policy_digest,created_at,finalized_at
from dorf.review_plans
where job_id=sqlc.arg(job_id) and revision=sqlc.arg(revision)
for update;

-- name: FinalizeReviewPlan :exec
update dorf.review_plans
set state='final',facts=sqlc.arg(facts)::jsonb,plan=sqlc.arg(plan)::jsonb,
    policy_digest=sqlc.arg(policy_digest)::text,finalized_at=clock_timestamp()
where job_id=sqlc.arg(job_id) and revision=sqlc.arg(revision) and policy_digest is null;

-- name: ListDeclaredReviewCheckNames :many
select name
from dorf.repository_commands
where job_id=sqlc.arg(job_id) and name in ('check','smoke')
order by name;

-- name: InsertReviewAction :exec
insert into dorf.actions(id,job_id,kind,state,scope_key)
values(sqlc.arg(id),sqlc.arg(job_id),sqlc.arg(kind),'pending',sqlc.arg(scope_key))
on conflict do nothing;

-- name: InsertReviewAgentRun :exec
insert into dorf.agent_runs(id,job_id,message_id,role,state,revision,capability,sandbox_id,submission_nonce)
values(sqlc.arg(id),sqlc.arg(job_id),sqlc.arg(message_id),sqlc.arg(role),'pending',
       sqlc.arg(revision)::text,sqlc.arg(capability)::text,sqlc.arg(sandbox_id),sqlc.arg(submission_nonce)::text)
on conflict do nothing;

-- name: AdvanceReviewPolicyPhase :execrows
update dorf.jobs
set workflow_phase=sqlc.arg(workflow_phase),workflow_attention=null
where id=sqlc.arg(job_id) and revision=sqlc.arg(revision) and workflow_phase='review-planning';

-- name: GetReviewRun :one
select *
from dorf.review_run_projection
where id=sqlc.arg(run_id);

-- name: GetReviewCurrentRevision :one
select revision
from dorf.jobs
where id=sqlc.arg(job_id);

-- name: ListReviewRuns :many
select sqlc.embed(p),coalesce(m.id,'') as feedback_message_id,(p.revision<>j.revision)::boolean as stale
from dorf.review_run_projection p
join dorf.jobs j on j.id=p.job_id
left join dorf.job_messages m
  on m.job_id=p.job_id and m.from_kind='agent' and m.from_id=p.id
where p.job_id=sqlc.arg(job_id) and p.revision=sqlc.arg(revision)
order by p.role;

-- name: ListAllReviewRuns :many
select sqlc.embed(p),coalesce(m.id,'') as feedback_message_id,(p.revision<>j.revision)::boolean as stale
from dorf.review_run_projection p
join dorf.jobs j on j.id=p.job_id
left join dorf.job_messages m
  on m.job_id=p.job_id and m.from_kind='agent' and m.from_id=p.id
where p.job_id=sqlc.arg(job_id) and p.revision<>''
order by p.revision,p.role;

-- name: InterruptAgentRun :execrows
update dorf.agent_runs
set state='interrupted',
    turn_outcome=case when turn_id is null then null else 'interrupted' end,
    attention=sqlc.arg(reason)::text,
    finished_at=case when started_at is null then null else coalesce(finished_at,clock_timestamp()) end
where id=sqlc.arg(run_id) and state in ('pending','submitting','active','uncertain');

-- name: GetReviewFeedbackRunForUpdate :one
select ar.job_id,coalesce(ar.revision,'') as revision,ar.role,ar.state,coalesce(ar.turn_id,'') as turn_id,
       coalesce(ar.capability,'') as capability,j.revision as current_revision,j.workflow_phase
from dorf.agent_runs ar
join dorf.jobs j on j.id=ar.job_id
where ar.id=sqlc.arg(run_id)
for update of j,ar;

-- name: ReviewBoundaryReady :one
select exists(
    select 1 from dorf.agent_runs ar
    join dorf.sandboxes s on s.id=ar.sandbox_id
    where ar.id=sqlc.arg(run_id)
      and ar.harness is not null and ar.thread_id is not null and ar.turn_id is not null
      and exists(select 1 from dorf.actions a where a.job_id=ar.job_id
          and a.kind='sandbox-create' and a.scope_key=s.id and a.state='succeeded')
      and exists(select 1 from dorf.actions a where a.job_id=ar.job_id
          and a.kind='provider-route-create' and a.scope_key=s.id and a.state='succeeded')
      and exists(select 1 from dorf.actions a where a.job_id=ar.job_id
          and a.kind='review-checkout' and a.scope_key=s.id and a.state='succeeded')
);

-- name: GetReviewFeedbackMessage :one
select id,job_id,from_kind,from_id,sequence,input,delivery_intent,
       coalesce(steer_target_turn_id,'') as steer_target_turn_id
from dorf.job_messages m
where m.job_id=sqlc.arg(job_id) and m.from_kind='agent' and m.from_id=sqlc.arg(run_id);

-- name: CountMissingReviewFeedback :one
select count(*)
from dorf.agent_runs ar
where ar.job_id=sqlc.arg(job_id) and ar.revision=sqlc.arg(revision)
  and not exists(
      select 1 from dorf.job_messages m
      where m.job_id=ar.job_id and m.from_kind='agent' and m.from_id=ar.id
  );

-- name: AdvanceJobToReviewFeedback :execrows
update dorf.jobs
set workflow_phase='review-feedback',workflow_attention=null
where id=sqlc.arg(job_id) and revision=sqlc.arg(revision) and workflow_phase='reviewing';

-- name: AdvanceJobReviewFeedbackToReady :execrows
update dorf.jobs
set workflow_phase='ready',workflow_attention=null
where id=sqlc.arg(job_id) and revision=sqlc.arg(revision) and workflow_phase='review-feedback';
