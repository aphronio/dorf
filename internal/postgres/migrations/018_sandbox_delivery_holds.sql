-- A retained delivery barrier is independent of executor process lifetime.
create table dorf.sandbox_delivery_holds (
    id text primary key,
    sandbox_id text not null references dorf.sandboxes(id),
    reason text not null check (reason='workspace_upgrade'),
    requested_at timestamptz not null default clock_timestamp(),
    released_at timestamptz
);
create unique index sandbox_one_delivery_hold on dorf.sandbox_delivery_holds(sandbox_id)
    where released_at is null;
insert into dorf.schema_migrations(name) values ('018_sandbox_delivery_holds.sql');
