-- Native writes invalidate older checkpoints before dispatch. No payload or
-- outcome is retained: the Harness remains the conversation authority.
alter table dorf.sessions
    add column native_revision bigint not null default 0 check (native_revision >= 0),
    add column native_pending_input_id text,
    add column native_pending_turn_id text,
    add constraint native_pending_one_kind check (
        native_pending_input_id is null or native_pending_turn_id is null
    );

-- An existing bound conversation must never be mistaken for an unused Thread.
update dorf.sessions set native_revision=1 where thread_id is not null;

-- Pre-contract snapshots cannot prove native continuity. Retire their local
-- references and completed recovery receipts; remote backup objects are untouched.
-- Never discard custody of an unfinished recovery.
do $$ begin
    if exists (select 1 from dorf.sandbox_recoveries where finished_at is null)
       or exists (select 1 from dorf.sandbox_delivery_holds
                  where reason='checkpoint_recovery' and released_at is null) then
        raise exception 'finish checkpoint recovery before replacing the native contract';
    end if;
end $$;
delete from dorf.sandbox_recoveries;
delete from dorf.sandbox_delivery_holds where reason='checkpoint_recovery';
delete from dorf.sandbox_checkpoints;
alter table dorf.sandbox_checkpoints rename column message_sequence to native_revision;
alter table dorf.sandbox_checkpoints drop column completed_turn_sequence;

-- Input and execution are now native facts. Published migration history remains
-- intact; these tables have no consumer in the replacement API.
drop table dorf.agent_runs;
drop table dorf.session_messages;

insert into dorf.schema_migrations(name) values ('027_native_session_guard.sql');
