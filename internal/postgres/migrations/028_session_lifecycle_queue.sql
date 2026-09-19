-- Run with the old API and worker stopped and native input settled. An idle
-- Session still has a sleeping lifecycle task even when it has no queued input.
-- Reattach that lifecycle to the new queue without changing resource custody.
do $$
declare
    retained record;
    replacement uuid;
begin
    if to_regclass('absurd.t_dorf_jobs') is not null then
        if exists (
            select 1 from dorf.sessions s
            join lateral (select task_id from dorf.session_tasks
                          where session_id=s.id order by sequence desc limit 1) current_task on true
            join absurd.t_dorf_jobs t on t.task_id::text=current_task.task_id
            where s.cleanup_state<>'complete' and (
                not s.admission_open or s.cleanup_state<>'pending'
                or s.execution_attention is not null
                or t.task_name<>'dorf-direct-job-v1' or t.state<>'sleeping'
                or exists (select 1 from dorf.actions a
                           where a.session_id=s.id and a.state<>'succeeded')
            )
        ) then
            raise exception 'settle Session lifecycle work before moving queues';
        end if;

        perform absurd.create_queue('dorf_sessions');
        for retained in
            select s.id,t.task_id
            from dorf.sessions s
            join lateral (select task_id from dorf.session_tasks
                          where session_id=s.id order by sequence desc limit 1) current_task on true
            join absurd.t_dorf_jobs t on t.task_id::text=current_task.task_id
            where s.admission_open and s.cleanup_state='pending'
        loop
            perform absurd.cancel_task('dorf_jobs',retained.task_id);
            select task_id into replacement from absurd.spawn_task(
                'dorf_sessions','dorf-direct-session-v1',
                jsonb_build_object('session_id',retained.id),
                jsonb_build_object('idempotency_key','direct-session:v1:' || retained.id,
                    'max_attempts',5,'retry_strategy',jsonb_build_object(
                        'kind','exponential','base_seconds',5,'factor',2,'max_seconds',60)));
            -- Attachments are queue execution history, not resource/effect receipts.
            delete from dorf.session_tasks where session_id=retained.id;
            insert into dorf.session_tasks(session_id,sequence,task_id,task_name)
            values(retained.id,1,replacement::text,'dorf-direct-session-v1');
        end loop;

        -- Closed Sessions need only their cleanup/resource receipts. Do not leave
        -- task or retry references pointing into a queue no worker consumes.
        delete from dorf.session_tasks a using absurd.t_dorf_jobs t
            where a.task_id=t.task_id::text;
        delete from dorf.session_retry_requests r using absurd.t_dorf_jobs t
            where r.task_id=t.task_id::text;
    end if;
end $$;
insert into dorf.schema_migrations(name) values ('028_session_lifecycle_queue.sql');
