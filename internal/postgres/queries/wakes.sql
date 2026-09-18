-- name: GetSessionExecutionWakeRevision :one
select coalesce(w.revision,0)::bigint as revision
from dorf.sessions j
left join dorf.session_execution_wakes w on w.session_id=j.id
where j.id=sqlc.arg(session_id);

-- name: EnsureSessionExecutionWake :exec
insert into dorf.session_execution_wakes(session_id)
values(sqlc.arg(session_id))
on conflict(session_id) do nothing;

-- name: LockSessionExecutionWake :one
select revision
from dorf.session_execution_wakes
where session_id=sqlc.arg(session_id)
for update;

-- name: GetSessionExecutionWakeCause :one
select revision
from dorf.session_execution_wake_causes
where session_id=sqlc.arg(session_id) and cause_key=sqlc.arg(cause_key);

-- name: SetSessionExecutionWakeRevision :execrows
update dorf.session_execution_wakes
set revision=sqlc.arg(revision)
where session_id=sqlc.arg(session_id);

-- name: InsertSessionExecutionWakeCause :exec
insert into dorf.session_execution_wake_causes(session_id,cause_key,revision)
values(sqlc.arg(session_id),sqlc.arg(cause_key),sqlc.arg(revision));

-- name: GetNativeTerminalWakeBinding :one
select j.id as session_id,s.id as sandbox_id,coalesce(j.thread_id,'') as thread_id,
       j.admission_open,j.cleanup_state
from dorf.sessions j join dorf.sandboxes s on s.session_id=j.id
where j.id=sqlc.arg(session_id) and s.id=sqlc.arg(sandbox_id);
