-- name: BeginSandboxActivity :execrows
update dorf.jobs set sandbox_last_active_at=null where id=$1;

-- name: FinishSandboxActivity :execrows
update dorf.jobs set sandbox_last_active_at=clock_timestamp() where id=$1;

-- name: SandboxIdleFor :one
update dorf.jobs j
set sandbox_last_active_at=coalesce(j.sandbox_last_active_at,clock_timestamp())
where j.id=sqlc.arg(job_id)
returning coalesce(j.sandbox_last_active_at <= clock_timestamp() - make_interval(secs => sqlc.arg(seconds)::double precision) and not exists (
    select 1 from dorf.agent_runs ar
    where ar.job_id=sqlc.arg(job_id) and ar.state not in ('completed','failed','interrupted')
),false)::boolean as idle;
