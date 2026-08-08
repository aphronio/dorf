alter table dorf.jobs
    add column provider_gateway_state text,
    add column cleanup_attention text;

alter table dorf.jobs add constraint jobs_provider_gateway_state_absolute_check check (
    provider_gateway_state is null or
    (provider_gateway_state like '/%' and provider_gateway_state = btrim(provider_gateway_state) and
     provider_gateway_state !~ E'[\\n\\r]')
);

create unique index github_proposals_exact_outcome_identity
    on dorf.github_proposals(
        job_id,repository,installation_id,base_branch,head_branch,pr_number,proposed_revision
    );

create table dorf.job_outcomes (
    job_id text primary key references dorf.jobs(id),
    outcome text not null check (outcome in ('accepted','rejected','abandoned')),
    repository text not null,
    installation_id text not null,
    base_branch text not null,
    head_branch text not null,
    pr_number bigint not null check (pr_number > 0),
    pr_url text not null check (length(pr_url) > 0),
    proposed_revision text not null check (proposed_revision ~ '^[0-9a-f]{40}([0-9a-f]{24})?$'),
    observed_head text not null check (observed_head ~ '^[0-9a-f]{40}([0-9a-f]{24})?$'),
    observed_state text not null check (observed_state in ('open','closed')),
    observed_merged boolean not null,
    merge_commit_oid text check (merge_commit_oid is null or merge_commit_oid ~ '^[0-9a-f]{40}([0-9a-f]{24})?$'),
    observed_at timestamptz not null default clock_timestamp(),
    foreign key(job_id,repository,installation_id,base_branch,head_branch,pr_number,proposed_revision)
        references dorf.github_proposals(
            job_id,repository,installation_id,base_branch,head_branch,pr_number,proposed_revision
        ),
    check (
        (outcome='accepted' and observed_state='closed' and observed_merged and merge_commit_oid is not null) or
        (outcome='rejected' and observed_state='closed' and not observed_merged and merge_commit_oid is null) or
        (outcome='abandoned' and not observed_merged and merge_commit_oid is null)
    )
);

comment on column dorf.jobs.provider_gateway_state is
    'Resolved absolute non-secret Provider Gateway deployment/state locator captured at Job admission';
comment on column dorf.jobs.cleanup_attention is
    'Current actionable cleanup diagnostic; cleared as exact cleanup converges';
comment on table dorf.job_outcomes is
    'Immutable first-write-wins Job outcome bound to one exact retained GitHub proposal and observed PR state';
alter table dorf.sandboxes drop constraint if exists sandboxes_state_check;
alter table dorf.sandboxes add constraint sandboxes_state_check
    check (state in ('pending','created','deleted'));

alter table dorf.routes drop constraint if exists routes_state_check;
alter table dorf.routes add constraint routes_state_check
    check (state in ('pending','active','revoked'));
