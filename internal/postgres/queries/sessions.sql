-- name: GetSession :one
select coalesce(j.created_by_client_id,'') as created_by_client_id, coalesce(creator.name,'') as created_by_client_name,j.client_reference,
       p.harness,coalesce(j.thread_id,'') as thread_id,
       j.id,j.admission_key,j.agents_md,
       j.sandbox_profile,j.sandbox_profile_revision,j.provider_connection,j.model,j.reasoning_effort,j.keep_running,j.admission_open,
       j.cleanup_state,coalesce(current_task.task_id,'') as current_task_id,
       coalesce(current_task.task_name,'') as current_task_name,
       coalesce(j.execution_attention,'') as execution_attention,
       coalesce(j.execution_attention_source,'') as execution_attention_source,
       j.execution_attention_at,coalesce(j.cleanup_attention,'') as cleanup_attention,
       j.admitted_at,j.cleaned_at
from dorf.sessions j
join dorf.sandbox_profile_revisions p on p.name=j.sandbox_profile and p.definition_hash=j.sandbox_profile_revision
left join dorf.control_clients creator on creator.id=j.created_by_client_id
left join lateral (
    select task_id,task_name from dorf.session_tasks where session_id=j.id order by sequence desc limit 1
) current_task on true
where j.id=sqlc.arg(session_id);

-- name: ListSessions :many
select j.id,j.admitted_at,
       coalesce(j.created_by_client_id,'') as created_by_client_id,coalesce(creator.name,'') as created_by_client_name,j.client_reference
from dorf.sessions j
join dorf.sandbox_profile_revisions p on p.name=j.sandbox_profile and p.definition_hash=j.sandbox_profile_revision
left join dorf.control_clients creator on creator.id=j.created_by_client_id
where (
        not sqlc.arg(has_cursor)::boolean or
        j.admitted_at < sqlc.arg(cursor_admitted_at)::timestamptz or
        (j.admitted_at=sqlc.arg(cursor_admitted_at)::timestamptz and j.id < sqlc.arg(cursor_id)::text)
      )
order by j.admitted_at desc,j.id desc
limit sqlc.arg(page_size);

-- name: InsertAdmittedSession :execrows
insert into dorf.sessions(
    id,admission_key,agents_md,created_by_client_id,client_reference,
    sandbox_profile,sandbox_profile_revision,provider_connection,model,reasoning_effort,keep_running
)
values(
    sqlc.arg(id),sqlc.arg(admission_key),
    sqlc.arg(agents_md),nullif(sqlc.arg(created_by_client_id)::text,''),sqlc.arg(client_reference),
    sqlc.arg(sandbox_profile),sqlc.arg(sandbox_profile_revision),sqlc.arg(provider_connection),sqlc.arg(model),
    sqlc.arg(reasoning_effort),sqlc.arg(keep_running)
)
on conflict do nothing;

-- name: GetAdmittedSessionForUpdate :one
select id,admission_key,agents_md,sandbox_profile,provider_connection,
       model,reasoning_effort,client_reference,keep_running
from dorf.sessions
where admission_key=sqlc.arg(admission_key)
for update;

-- name: GetSessionAdmissionForUpdate :one
select j.admission_open,j.cleanup_state,
       p.harness,coalesce(j.thread_id,'') as thread_id
from dorf.sessions j
join dorf.sandbox_profile_revisions p on p.name=j.sandbox_profile and p.definition_hash=j.sandbox_profile_revision
where j.id=sqlc.arg(session_id)
for update of j;

-- name: GetSessionForSandboxActionAuthorization :one
select coalesce(j.created_by_client_id,'') as created_by_client_id, coalesce(creator.name,'') as created_by_client_name,j.client_reference,
       p.harness,coalesce(j.thread_id,'') as thread_id,
       j.id,j.admission_key,j.agents_md,
       j.sandbox_profile,j.sandbox_profile_revision,j.provider_connection,j.model,j.reasoning_effort,j.keep_running,j.admission_open,
       j.cleanup_state,coalesce(current_task.task_id,'') as current_task_id,
       coalesce(current_task.task_name,'') as current_task_name,
       coalesce(j.execution_attention,'') as execution_attention,
       coalesce(j.execution_attention_source,'') as execution_attention_source,
       j.execution_attention_at,coalesce(j.cleanup_attention,'') as cleanup_attention,
       j.admitted_at,j.cleaned_at
from dorf.sessions j
join dorf.sandbox_profile_revisions p on p.name=j.sandbox_profile and p.definition_hash=j.sandbox_profile_revision
left join dorf.control_clients creator on creator.id=j.created_by_client_id
left join lateral (
    select task_id,task_name from dorf.session_tasks where session_id=j.id order by sequence desc limit 1
) current_task on true
where j.id=sqlc.arg(session_id)
for update of j;

-- name: GetSessionSandboxProfileForUpdate :one
select sandbox_profile
from dorf.sessions
where id=sqlc.arg(session_id)
for update;

-- name: GetCurrentSessionTaskForUpdate :one
select coalesce(current_task.task_id,'') as task_id,
       coalesce(current_task.task_name,'') as task_name,
       coalesce(current_task.sequence,0)::bigint as sequence,
       j.admission_open,j.cleanup_state
from dorf.sessions j
left join lateral (
    select task_id,task_name,sequence
    from dorf.session_tasks where session_id=j.id order by sequence desc limit 1
) current_task on true
where j.id=sqlc.arg(session_id)
for update of j;

-- name: ListSessionTasks :many
select session_id,sequence,task_id,task_name,attached_at
from dorf.session_tasks
where session_id=sqlc.arg(session_id)
order by sequence;

-- name: InsertSessionTask :execrows
insert into dorf.session_tasks(session_id,sequence,task_id,task_name)
values(sqlc.arg(session_id),sqlc.arg(sequence),sqlc.arg(task_id),sqlc.arg(task_name))
on conflict(task_id) do nothing;

-- name: MarkCleanupScheduled :execrows
update dorf.sessions
set cleanup_state='scheduled'
where id=sqlc.arg(session_id) and not admission_open and cleanup_state='requested';

-- name: SetExecutionAttention :execrows
update dorf.sessions
set execution_attention=sqlc.arg(detail),
    execution_attention_source=sqlc.arg(source),
    execution_attention_at=clock_timestamp()
where id=sqlc.arg(session_id)
  and (execution_attention_source is null or execution_attention_source=sqlc.arg(source));

-- name: ClearExecutionAttention :execrows
update dorf.sessions
set execution_attention=null,execution_attention_source=null,execution_attention_at=null
where id=sqlc.arg(session_id) and execution_attention_source=sqlc.arg(source);

-- name: SetCleanupAttention :execrows
update dorf.sessions
set cleanup_attention=nullif(sqlc.arg(detail)::text,'')
where id=sqlc.arg(session_id) and cleanup_state<>'complete';
