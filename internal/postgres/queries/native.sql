-- name: GetNativeState :one
select native_revision,coalesce(native_pending_input_id,'')::text as pending_input_id,
       coalesce(native_pending_turn_id,'')::text as pending_turn_id
from dorf.sessions where id=sqlc.arg(session_id);

-- name: BindNativeThread :execrows
update dorf.sessions set thread_id=sqlc.arg(thread_id)
where dorf.sessions.id=sqlc.arg(session_id) and admission_open and cleanup_state='pending'
  and (thread_id=sqlc.arg(thread_id) or (native_revision=0 and native_pending_input_id is null
       and native_pending_turn_id is null));

-- name: BeginNativeMutation :one
update dorf.sessions set native_revision=native_revision+1,
    native_pending_input_id=nullif(sqlc.arg(input_id)::text,''),
    native_pending_turn_id=nullif(sqlc.arg(turn_id)::text,'')
where dorf.sessions.id=sqlc.arg(session_id) and admission_open and cleanup_state='pending'
  and thread_id=sqlc.arg(thread_id)
  and native_pending_input_id is null and native_pending_turn_id is null
  and not exists (
      select 1 from dorf.sandbox_delivery_holds h join dorf.sandboxes s on s.id=h.sandbox_id
      where s.session_id=dorf.sessions.id and h.released_at is null
  )
returning native_revision;

-- name: FinishNativeMutation :execrows
update dorf.sessions set native_pending_input_id=null,native_pending_turn_id=null
where id=sqlc.arg(session_id) and native_revision=sqlc.arg(revision);
