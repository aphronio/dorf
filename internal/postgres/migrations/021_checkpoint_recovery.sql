alter table dorf.sandbox_delivery_holds
    drop constraint sandbox_delivery_holds_reason_check;
alter table dorf.sandbox_delivery_holds
    add constraint sandbox_delivery_holds_reason_check
    check (reason in ('workspace_upgrade','checkpoint_recovery'));

-- Recovery receipts retain only facts needed to reconcile one exact published
-- checkpoint into a fresh provider resource. Current custody remains the
-- logical Sandbox's active_resource_id.
create table dorf.sandbox_recoveries (
    id text primary key references dorf.sandbox_delivery_holds(id),
    sandbox_id text not null references dorf.sandboxes(id),
    checkpoint_repository text not null,
    checkpoint_snapshot_id text not null,
    source_resource_id text not null,
    destination_resource_id text not null,
    requested_at timestamptz not null default clock_timestamp(),
    verified_at timestamptz,
    finished_at timestamptz,
    foreign key(checkpoint_repository,checkpoint_snapshot_id)
        references dorf.sandbox_checkpoints(repository,snapshot_id),
    foreign key(sandbox_id,source_resource_id)
        references dorf.sandbox_resources(sandbox_id,id),
    foreign key(sandbox_id,destination_resource_id)
        references dorf.sandbox_resources(sandbox_id,id),
    check (destination_resource_id<>source_resource_id),
    check (finished_at is null or verified_at is not null)
);
create unique index sandbox_recoveries_one_unfinished
    on dorf.sandbox_recoveries(sandbox_id)
    where finished_at is null;

insert into dorf.schema_migrations(name) values ('021_checkpoint_recovery.sql');
