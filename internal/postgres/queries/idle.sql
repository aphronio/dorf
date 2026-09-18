-- name: BeginSandboxActivity :execrows
update dorf.sessions set sandbox_last_active_at=null where id=$1;

-- name: FinishSandboxActivity :execrows
update dorf.sessions set sandbox_last_active_at=clock_timestamp() where id=$1;

-- name: SandboxIdleFor :one
update dorf.sessions j
set sandbox_last_active_at=coalesce(j.sandbox_last_active_at,clock_timestamp())
where j.id=sqlc.arg(session_id)
returning coalesce(j.sandbox_last_active_at <= clock_timestamp() - make_interval(secs => sqlc.arg(seconds)::double precision) and not exists (
    select 1 from dorf.agent_runs ar
    where ar.session_id=sqlc.arg(session_id) and ar.state not in ('completed','failed','interrupted')
) and not exists (
    select 1 from dorf.sandbox_delivery_holds h join dorf.sandboxes s on s.id=h.sandbox_id
    where s.session_id=j.id and h.released_at is null
),false)::boolean as idle;
