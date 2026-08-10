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
insert into dorf.agent_runs(id,job_id,message_id,role,state,revision,capability)
values(sqlc.arg(id),sqlc.arg(job_id),sqlc.arg(message_id),sqlc.arg(role),'pending',
       sqlc.arg(revision)::text,sqlc.arg(capability)::text)
on conflict do nothing;

-- name: InsertReviewResource :exec
insert into dorf.review_resources(
    run_id,job_id,revision,sandbox_name,ownership_nonce,submission_nonce,
    sandbox_create_action_id,route_create_action_id,materialize_action_id,route_revoke_action_id,sandbox_delete_action_id
) values(
    sqlc.arg(run_id),sqlc.arg(job_id),sqlc.arg(revision),sqlc.arg(sandbox_name),sqlc.arg(ownership_nonce),
    sqlc.arg(submission_nonce),sqlc.arg(sandbox_create_action_id),sqlc.arg(route_create_action_id),
    sqlc.arg(materialize_action_id),sqlc.arg(route_revoke_action_id),sqlc.arg(sandbox_delete_action_id)
)
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

-- name: ListCleanupReviewRuns :many
select p.*
from dorf.review_run_projection p
join dorf.review_resources rr on rr.run_id=p.id
where rr.job_id=sqlc.arg(job_id)
order by rr.revision,p.role;

-- name: GetReviewActionID :one
select case sqlc.arg(kind)::text
    when 'sandbox-create' then rr.sandbox_create_action_id
    when 'provider-route-create' then rr.route_create_action_id
    when 'review-workspace-create' then rr.materialize_action_id
    when 'provider-route-revoke' then rr.route_revoke_action_id
    when 'sandbox-delete' then rr.sandbox_delete_action_id
end::text as action_id
from dorf.review_resources rr
where rr.run_id=sqlc.arg(run_id);

-- name: GetReviewActionForUpdate :one
select id,job_id,kind,state,coalesce(external_id,'') as external_id,
       coalesce(external_outcome,'') as external_outcome,scope_key
from dorf.actions
where id=sqlc.arg(action_id)
for update;

-- name: GetReviewRunStateForUpdate :one
select ar.state
from dorf.agent_runs ar
join dorf.review_resources rr on rr.run_id=ar.id
where ar.id=sqlc.arg(run_id) and ar.capability=sqlc.arg(capability)::text
for update of ar;

-- name: InterruptReviewAgentRun :execrows
update dorf.agent_runs
set state='interrupted',
    turn_outcome=case when turn_id is null then null else 'interrupted' end,
    attention=sqlc.arg(reason)::text,
    finished_at=case when started_at is null then null else coalesce(finished_at,clock_timestamp()) end
where id=sqlc.arg(run_id) and state in ('pending','submitting','active','uncertain');

-- name: VerifyReviewPostState :execrows
update dorf.review_resources rr
set post_review_state='verified',post_review_verified_at=coalesce(post_review_verified_at,clock_timestamp())
from dorf.agent_runs ar
where rr.run_id=sqlc.arg(run_id) and ar.id=rr.run_id and rr.sandbox_state='created' and rr.route_state='active'
  and rr.checkout_state='verified' and rr.revision=sqlc.arg(revision) and rr.revision_tree=sqlc.arg(revision_tree)::text
  and ar.capability='immutable-read-only';

-- name: GetReviewFeedbackRunForUpdate :one
select ar.job_id,coalesce(ar.revision,'') as revision,ar.role,ar.state,coalesce(ar.turn_id,'') as turn_id,
       coalesce(ar.capability,'') as capability,j.revision as current_revision,j.workflow_phase
from dorf.agent_runs ar
join dorf.jobs j on j.id=ar.job_id
where ar.id=sqlc.arg(run_id)
for update of j,ar;

-- name: ReviewBoundaryReady :one
select exists(
    select 1 from dorf.review_resources rr
    join dorf.agent_runs ar on ar.id=rr.run_id
    where rr.run_id=sqlc.arg(run_id) and rr.sandbox_state='created' and rr.route_state='active'
      and rr.checkout_state='verified' and rr.post_review_state='verified'
      and rr.revision_tree is not null and ar.harness is not null and ar.thread_id is not null and ar.turn_id is not null
);

-- name: GetReviewFeedbackMessage :one
select id,job_id,from_kind,from_id,sequence,input,delivery_intent,
       coalesce(steer_target_turn_id,'') as steer_target_turn_id
from dorf.job_messages m
where m.job_id=sqlc.arg(job_id) and m.from_kind='agent' and m.from_id=sqlc.arg(run_id);

-- name: CountMissingReviewFeedback :one
select count(*)
from dorf.review_resources rr
where rr.job_id=sqlc.arg(job_id) and rr.revision=sqlc.arg(revision)
  and not exists(
      select 1 from dorf.job_messages m
      where m.job_id=rr.job_id and m.from_kind='agent' and m.from_id=rr.run_id
  );

-- name: AdvanceJobToReviewFeedback :execrows
update dorf.jobs
set workflow_phase='review-feedback',workflow_attention=null
where id=sqlc.arg(job_id) and revision=sqlc.arg(revision) and workflow_phase='reviewing';

-- name: AdvanceJobReviewFeedbackToReady :execrows
update dorf.jobs
set workflow_phase='ready',workflow_attention=null
where id=sqlc.arg(job_id) and revision=sqlc.arg(revision) and workflow_phase='review-feedback';
