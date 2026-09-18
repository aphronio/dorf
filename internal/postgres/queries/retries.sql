-- name: LockSessionRetryRequest :exec
select pg_advisory_xact_lock(hashtextextended('dorf-session-retry:' || sqlc.arg(request_key)::text,0));

-- name: GetSessionRetryRequest :one
select request_key,session_id,task_id,run_id,attempt
from dorf.session_retry_requests
where request_key=sqlc.arg(request_key);

-- name: InsertSessionRetryRequest :exec
insert into dorf.session_retry_requests(request_key,session_id,task_id,run_id,attempt)
values(sqlc.arg(request_key),sqlc.arg(session_id),sqlc.arg(task_id),sqlc.arg(run_id),sqlc.arg(attempt));
