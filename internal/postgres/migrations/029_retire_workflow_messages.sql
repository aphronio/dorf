-- Repair the upgrade path through 026, whose temporary input-kind constraint
-- predates complete input-table retirement in 027. Run after the Session rename
-- and before 026; databases already past 027 have nothing left to transform.
do $$
begin
    if to_regclass('dorf.session_messages') is not null then
        if exists (select 1 from dorf.sessions
                   where workflow_name<>'' and cleanup_state<>'complete') then
            raise exception 'finish retired application cleanup before the native Session upgrade';
        end if;
        delete from dorf.agent_runs r using dorf.session_messages m
            where r.message_id=m.id and m.from_kind='workflow';
        delete from dorf.session_messages where from_kind='workflow';
    end if;
end $$;
insert into dorf.schema_migrations(name) values ('029_retire_workflow_messages.sql');
