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
select ar.session_id,ar.sandbox_id,coalesce(ar.thread_id,'') as thread_id,
       coalesce(ar.turn_id,'') as turn_id,j.admission_open,j.cleanup_state
from dorf.agent_runs ar
join dorf.sessions j on j.id=ar.session_id
join dorf.sandboxes s on s.id=ar.sandbox_id and s.session_id=ar.session_id
where ar.id=sqlc.arg(run_id);
