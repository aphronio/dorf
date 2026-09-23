-- A branch is a new Session with a held, destination-owned Sandbox. The
-- immutable checkpoint remains owned by its source and is read only at restore.
alter table dorf.sandbox_delivery_holds
    drop constraint sandbox_delivery_holds_reason_check;
alter table dorf.sandbox_delivery_holds
    add constraint sandbox_delivery_holds_reason_check
    check (reason in ('workspace_upgrade','checkpoint_recovery','checkpoint_branch'));

create table dorf.sandbox_branches (
    id text primary key references dorf.sandbox_delivery_holds(id),
    source_session_id text not null references dorf.sessions(id),
    destination_session_id text not null unique references dorf.sessions(id),
    destination_sandbox_id text not null unique references dorf.sandboxes(id),
    checkpoint_repository text not null,
    checkpoint_snapshot_id text not null,
    thread_id text not null check (length(trim(thread_id))>0),
    requested_at timestamptz not null default clock_timestamp(),
    restored_at timestamptz,
    release_requested_at timestamptz,
    ready_at timestamptz,
    foreign key(checkpoint_repository,checkpoint_snapshot_id)
        references dorf.sandbox_checkpoints(repository,snapshot_id),
    check (release_requested_at is null or restored_at is not null),
    check (ready_at is null or release_requested_at is not null)
);

insert into dorf.schema_migrations(name) values ('030_checkpoint_branches.sql');
