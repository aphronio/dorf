-- A direct Job owns one continuing native conversation. Historical application
-- runs retain their own attribution and are not converted into direct Jobs.
alter table dorf.jobs
    add column thread_harness text,
    add column thread_id text,
    add constraint job_thread_binding check (
        (thread_harness is null and thread_id is null) or
        (thread_harness is not null and length(trim(thread_harness)) > 0
         and thread_id is not null and length(trim(thread_id)) > 0)
    );

do $$
begin
    if exists (
        select ar.job_id
        from dorf.agent_runs ar join dorf.jobs j on j.id=ar.job_id
        where j.workflow_name='' and j.workflow_revision='' and ar.thread_id is not null
        group by ar.job_id
        having count(distinct (ar.harness,ar.thread_id)) > 1
            or bool_or(ar.harness is null or length(trim(ar.harness))=0 or length(trim(ar.thread_id))=0)
    ) then
        raise exception 'direct Job has conflicting or incomplete retained Thread bindings';
    end if;
end $$;

update dorf.jobs j
set thread_harness=bound.harness,thread_id=bound.thread_id
from (
    select distinct job_id,harness,thread_id from dorf.agent_runs where thread_id is not null
) bound
where j.id=bound.job_id and j.workflow_name='' and j.workflow_revision='';

insert into dorf.schema_migrations(name) values ('024_job_thread.sql');
