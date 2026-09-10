alter table dorf.jobs
    add column created_by_client_id text references dorf.control_clients(id),
    add column client_reference text not null default '';

insert into dorf.schema_migrations(name) values ('007_job_client_attribution.sql');
