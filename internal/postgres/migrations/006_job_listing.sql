create index jobs_by_admitted_at_id
    on dorf.jobs(admitted_at desc,id desc)
    include(workflow_name,workflow_revision);

insert into dorf.schema_migrations(name) values ('006_job_listing.sql');
