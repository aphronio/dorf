alter table dorf.jobs
    add column github_repository text,
    add column github_installation_id text,
    add column base_branch text,
    add column publication_task_id text,
    add column publication_attempt integer not null default 0;

alter table dorf.jobs add constraint jobs_github_authority_complete_check check (
    (github_repository is null and github_installation_id is null and base_branch is null) or
    (github_repository ~ '^[a-z0-9]([a-z0-9-]{0,37}[a-z0-9])?/[a-z0-9][a-z0-9_.-]*$' and
     github_installation_id ~ '^[1-9][0-9]*$' and
     length(base_branch) > 0 and base_branch <> branch)
);
alter table dorf.jobs add constraint jobs_publication_attempt_check
    check (publication_attempt >= 0);
alter table dorf.jobs add constraint jobs_github_authority_identity_key
    unique(id,github_repository,github_installation_id,base_branch,branch);

alter table dorf.jobs drop constraint if exists jobs_workflow_phase_check;
alter table dorf.jobs add constraint jobs_workflow_phase_check check (
    workflow_phase in (
        'setup','implementing','committing','checking','repairing',
        'review-activation','review-planning','review-triage','reviewing','review-repairing',
        'ready','publishing','publication-blocked','published','blocked'
    )
);

alter table dorf.actions drop constraint if exists actions_kind_check;
alter table dorf.actions add constraint actions_kind_check check (kind in (
    'sandbox-create','repository-clone','repository-setup','repository-commit',
    'repository-push','github-pull-request','review-workspace-create',
    'provider-route-create','codex-session-start','codex-turn-start',
    'provider-route-revoke','sandbox-delete'
));

create table dorf.github_proposals (
    job_id text primary key references dorf.jobs(id),
    repository text not null,
    installation_id text not null,
    base_branch text not null,
    head_branch text not null,
    pr_number bigint not null check (pr_number > 0),
    pr_url text not null check (length(pr_url) > 0),
    proposed_revision text not null check (proposed_revision ~ '^[0-9a-f]{40}([0-9a-f]{24})?$'),
    observed_remote_head text not null check (observed_remote_head ~ '^[0-9a-f]{40}([0-9a-f]{24})?$'),
    body_digest text not null check (body_digest ~ '^[0-9a-f]{64}$'),
    observed_at timestamptz not null default clock_timestamp(),
    foreign key(job_id,repository,installation_id,base_branch,head_branch)
        references dorf.jobs(id,github_repository,github_installation_id,base_branch,branch)
);

comment on table dorf.github_proposals is
    'One non-secret GitHub proposal projection per Job; freshness is exact proposed Revision, remote head, and body digest';
comment on column dorf.jobs.github_repository is
    'Canonical lower-case GitHub owner/repository, immutable once bound';
comment on column dorf.jobs.base_branch is
    'Explicit immutable GitHub proposal base; never inferred from repository defaults';
