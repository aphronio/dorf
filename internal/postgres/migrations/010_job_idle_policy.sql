alter table dorf.jobs add column keep_running boolean not null default false;

insert into dorf.schema_migrations(name) values ('010_job_idle_policy.sql');
