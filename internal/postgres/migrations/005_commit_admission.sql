alter table dorf.jobs drop constraint if exists jobs_workflow_phase_check;
alter table dorf.jobs add constraint jobs_workflow_phase_check check (
    workflow_phase in ('setup','implementing','committing','checking','repairing','ready','blocked')
);

comment on column dorf.jobs.workflow_phase is
    'Explicit coding phase; committing atomically closes steering before a Revision Action';
