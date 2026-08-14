alter table dorf.jobs
    add column sandbox_profile text;

-- Every Job admitted before provider selection existed was owned by the
-- process-wide Incus profile.
update dorf.jobs
set sandbox_profile='incus';

alter table dorf.jobs
    alter column sandbox_profile set not null,
    add constraint jobs_sandbox_profile_check check (length(trim(sandbox_profile)) > 0);

insert into dorf.schema_migrations(name) values ('002_sandbox_profile.sql');
