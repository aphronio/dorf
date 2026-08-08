alter table dorf.jobs drop constraint if exists jobs_workflow_phase_check;

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
