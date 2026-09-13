alter table dorf.jobs add column sandbox_last_active_at timestamptz default clock_timestamp();

insert into dorf.schema_migrations(name) values ('012_sandbox_idle_grace.sql');
