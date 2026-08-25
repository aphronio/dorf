-- Admit client-directed Jobs without disguising them as a workflow. Existing
-- workflow identities remain an exact name/revision pair, while the absent
-- pair is represented by two empty non-null values in the retained Job row.

alter table dorf.jobs drop constraint if exists jobs_workflow_name_check;
alter table dorf.jobs drop constraint if exists jobs_workflow_revision_check;
alter table dorf.jobs add constraint jobs_workflow_identity_check check (
    (workflow_name='' and workflow_revision='') or
    (workflow_name in ('coding-to-proposal','codebase-investigation') and length(trim(workflow_revision))>0)
);

alter table dorf.agent_runs drop constraint if exists agent_runs_role_check;
alter table dorf.agent_runs add constraint agent_runs_role_check check (
    role in ('implement','investigate','direct','general','browser-ui','auth-authority','performance','critical-boundary')
);
alter table dorf.agent_runs drop constraint if exists agent_runs_role_binding_check;
alter table dorf.agent_runs add constraint agent_runs_role_binding_check check (
    (role='implement' and capability is null and submission_nonce is null) or
    (role='investigate' and input_revision is not null and capability='repository-read-report' and submission_nonce is null) or
    (role='direct' and input_revision is null and capability is null and submission_nonce is null) or
    (role in ('general','browser-ui','auth-authority','performance','critical-boundary') and
     input_revision is not null and capability='immutable-read-only' and
     submission_nonce ~ '^[0-9a-f]{64}$')
);

insert into dorf.schema_migrations(name) values ('003_client_directed_jobs.sql');
