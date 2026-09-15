-- Successful Sandbox checkpoints retain only immutable recovery facts. Upload
-- attempts and failures remain in diagnostics rather than durable product state.
alter table dorf.sandbox_upgrades add constraint sandbox_upgrades_sandbox_id_id_key
    unique(sandbox_id,id);

create table dorf.sandbox_checkpoints (
    repository text not null check (
        length(repository) between 1 and 256 and repository=trim(repository)
    ),
    snapshot_id text not null check (snapshot_id ~ '^[0-9a-f]{64}$'),
    sandbox_id text not null references dorf.sandboxes(id),
    resource_id text not null,
    profile_name text not null,
    profile_revision text not null,
    effective_upgrade_id text,
    last_activity_at timestamptz not null,
    message_sequence bigint not null check (message_sequence >= 0),
    completed_turn_sequence bigint not null check (
        completed_turn_sequence >= 0 and completed_turn_sequence <= message_sequence
    ),
    delivery_hold_count bigint not null check (delivery_hold_count >= 0),
    cleanup boolean not null,
    published_at timestamptz not null default clock_timestamp(),
    publication_sequence bigint generated always as identity unique,
    primary key(repository,snapshot_id),
    foreign key(sandbox_id,resource_id)
        references dorf.sandbox_resources(sandbox_id,id),
    foreign key(profile_name,profile_revision)
        references dorf.sandbox_profile_revisions(name,definition_hash),
    foreign key(sandbox_id,effective_upgrade_id)
        references dorf.sandbox_upgrades(sandbox_id,id)
);
create index sandbox_checkpoints_latest
    on dorf.sandbox_checkpoints(sandbox_id,publication_sequence desc);

create function dorf.immutable_sandbox_checkpoint() returns trigger language plpgsql as $$
begin
    raise exception 'Sandbox checkpoints are immutable';
end;
$$;
create trigger immutable_sandbox_checkpoint before update on dorf.sandbox_checkpoints
for each row execute function dorf.immutable_sandbox_checkpoint();

insert into dorf.schema_migrations(name) values ('020_sandbox_checkpoints.sql');
