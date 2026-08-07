alter table dorf.jobs drop constraint if exists jobs_workflow_phase_check;
alter table dorf.jobs add constraint jobs_workflow_phase_check check (
    workflow_phase in (
        'setup','implementing','committing','checking','repairing',
        'review-activation','review-planning','review-triage','reviewing','review-repairing',
        'ready','blocked'
    )
);

comment on column dorf.jobs.workflow_phase is
    'review-activation is the durable post-Checks boundary where requested Roles are atomically bound before policy evaluation';
