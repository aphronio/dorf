-- Bind each caller retry request to the exact Absurd attempt it scheduled.
-- The row and absurd.retry_task effect commit in one PostgreSQL transaction.

create table dorf.job_retry_requests (
    request_key text primary key check (length(request_key) between 1 and 255 and request_key=trim(request_key)),
    job_id text not null references dorf.jobs(id),
    task_id text not null check (task_id ~ '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$'),
    run_id text not null unique check (run_id ~ '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$'),
    attempt integer not null check (attempt > 0)
);

insert into dorf.schema_migrations(name) values ('005_job_retry_requests.sql');
