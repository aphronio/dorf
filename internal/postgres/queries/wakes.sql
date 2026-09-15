-- name: GetJobExecutionWakeRevision :one
select coalesce(w.revision,0)::bigint as revision
from dorf.jobs j
left join dorf.job_execution_wakes w on w.job_id=j.id
where j.id=sqlc.arg(job_id);

-- name: EnsureJobExecutionWake :exec
insert into dorf.job_execution_wakes(job_id)
values(sqlc.arg(job_id))
on conflict(job_id) do nothing;

-- name: LockJobExecutionWake :one
select revision
from dorf.job_execution_wakes
where job_id=sqlc.arg(job_id)
for update;

-- name: GetJobExecutionWakeCause :one
select revision
from dorf.job_execution_wake_causes
where job_id=sqlc.arg(job_id) and cause_key=sqlc.arg(cause_key);

-- name: SetJobExecutionWakeRevision :execrows
update dorf.job_execution_wakes
set revision=sqlc.arg(revision)
where job_id=sqlc.arg(job_id);

-- name: InsertJobExecutionWakeCause :exec
insert into dorf.job_execution_wake_causes(job_id,cause_key,revision)
values(sqlc.arg(job_id),sqlc.arg(cause_key),sqlc.arg(revision));

-- name: GetNativeTerminalWakeBinding :one
select ar.job_id,ar.sandbox_id,coalesce(ar.thread_id,'') as thread_id,
       coalesce(ar.turn_id,'') as turn_id,j.admission_open,j.cleanup_state
from dorf.agent_runs ar
join dorf.jobs j on j.id=ar.job_id
join dorf.sandboxes s on s.id=ar.sandbox_id and s.job_id=ar.job_id
where ar.id=sqlc.arg(run_id);
