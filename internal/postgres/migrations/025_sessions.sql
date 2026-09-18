-- Rename custody without changing accepted identities or published migrations.
alter table dorf.jobs rename to sessions;
alter table dorf.job_tasks rename to session_tasks;
alter table dorf.job_messages rename to session_messages;
alter table dorf.job_retry_requests rename to session_retry_requests;
alter table dorf.job_execution_wakes rename to session_execution_wakes;
alter table dorf.job_execution_wake_causes rename to session_execution_wake_causes;
alter table dorf.session_tasks rename column job_id to session_id;
alter table dorf.session_messages rename column job_id to session_id;
alter table dorf.session_retry_requests rename column job_id to session_id;
alter table dorf.session_execution_wakes rename column job_id to session_id;
alter table dorf.session_execution_wake_causes rename column job_id to session_id;
alter table dorf.actions rename column job_id to session_id;
alter table dorf.sandboxes rename column job_id to session_id;
alter table dorf.agent_runs rename column job_id to session_id;

-- The immutable admitted profile already owns Harness selection.
do $$
begin
    if exists (
        select 1 from dorf.sessions s
        join dorf.sandbox_profile_revisions p
          on p.name=s.sandbox_profile and p.definition_hash=s.sandbox_profile_revision
        where s.thread_harness is not null and s.thread_harness<>p.harness
    ) then
        raise exception 'Session Thread Harness disagrees with its admitted profile';
    end if;
end $$;

alter table dorf.sessions drop constraint job_thread_binding;
alter table dorf.sessions drop column thread_harness;
alter table dorf.sessions add constraint session_thread
    check (thread_id is null or length(trim(thread_id))>0);

insert into dorf.schema_migrations(name) values ('025_sessions.sql');
