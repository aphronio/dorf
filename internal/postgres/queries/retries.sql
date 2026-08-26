-- name: LockJobRetryRequest :exec
select pg_advisory_xact_lock(hashtextextended('dorf-job-retry:' || sqlc.arg(request_key)::text,0));

-- name: GetJobRetryRequest :one
select request_key,job_id,task_id,run_id,attempt
from dorf.job_retry_requests
where request_key=sqlc.arg(request_key);

-- name: InsertJobRetryRequest :exec
insert into dorf.job_retry_requests(request_key,job_id,task_id,run_id,attempt)
values(sqlc.arg(request_key),sqlc.arg(job_id),sqlc.arg(task_id),sqlc.arg(run_id),sqlc.arg(attempt));
