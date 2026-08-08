select absurd.create_queue('dorf_jobs');

alter table dorf.jobs drop constraint if exists jobs_workflow_phase_check;

insert into dorf.review_plans(job_id,revision,state)
select id,revision,'pending'
from dorf.jobs
where workflow_phase='review-activation';

select absurd.emit_event(
    'dorf_jobs',
    pending_wait.event_name,
    jsonb_build_object('job_id',job.id,'sequence',right(pending_wait.event_name,20)::bigint)
)
from dorf.jobs job
join absurd.w_dorf_jobs pending_wait on pending_wait.task_id=job.task_id::uuid
where job.workflow_phase='review-activation'
  and right(pending_wait.event_name,20) ~ '^[0-9]{20}$'
  and pending_wait.event_name='dorf.job-message:'||job.id||':'||right(pending_wait.event_name,20);

update dorf.jobs
set workflow_phase='review-planning',workflow_attention=null
where workflow_phase='review-activation';

alter table dorf.jobs add constraint jobs_workflow_phase_check check (
    workflow_phase in (
        'setup','implementing','committing','checking','repairing',
        'review-planning','review-triage','reviewing','review-repairing',
        'ready','publishing','publication-blocked','published','blocked'
    )
);

alter table dorf.review_plans
    drop column if exists requested_by_run_id,
    drop column if exists requested_roles;

comment on column dorf.jobs.workflow_phase is
    'Verified Checks enter durable review planning directly; the admitted Job task advances policy, selected reviews, repair, and exact-Revision publication';
