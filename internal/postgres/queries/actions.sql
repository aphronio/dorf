-- name: InsertActionIfAbsent :exec
insert into dorf.actions(id,job_id,kind,state)
values(sqlc.arg(id),sqlc.arg(job_id),sqlc.arg(kind),'pending')
on conflict do nothing;

-- name: InsertMessageActionIfAbsent :exec
insert into dorf.actions(id,job_id,message_id,kind,state)
values(sqlc.arg(id),sqlc.arg(job_id),sqlc.arg(message_id),sqlc.arg(kind),'pending')
on conflict do nothing;

-- name: InsertMessageAction :exec
insert into dorf.actions(id,job_id,message_id,kind,state)
values(sqlc.arg(id),sqlc.arg(job_id),sqlc.arg(message_id),sqlc.arg(kind),'pending');

-- name: GetMessageActionID :one
select id
from dorf.actions
where message_id=sqlc.arg(message_id) and kind=sqlc.arg(kind);

-- name: ReserveMainSandbox :execrows
insert into dorf.sandboxes(job_id,action_id,incus_name,state)
values(sqlc.arg(job_id),sqlc.arg(action_id),sqlc.arg(incus_name),'pending')
on conflict(job_id) do update set action_id=dorf.sandboxes.action_id
where dorf.sandboxes.action_id=excluded.action_id
  and dorf.sandboxes.incus_name=excluded.incus_name;

-- name: ReserveMainRoute :execrows
insert into dorf.routes(job_id,action_id,route_id,state)
values(sqlc.arg(job_id),sqlc.arg(action_id),sqlc.arg(route_id),'pending')
on conflict(job_id) do update set action_id=dorf.routes.action_id
where dorf.routes.action_id=excluded.action_id
  and dorf.routes.route_id=excluded.route_id;

-- name: GetUnscopedActionForUpdate :one
select id,job_id,coalesce(message_id,'') as message_id,kind,state,
       coalesce(external_id,'') as external_id,
       coalesce(external_outcome,'') as external_outcome,scope_key
from dorf.actions
where job_id=sqlc.arg(job_id) and kind=sqlc.arg(kind)
  and message_id is null and scope_key=''
for update;

-- name: GetActionForUpdate :one
select id,job_id,coalesce(message_id,'') as message_id,kind,state,
       coalesce(external_id,'') as external_id,
       coalesce(external_outcome,'') as external_outcome,scope_key
from dorf.actions
where id=sqlc.arg(id) and job_id=sqlc.arg(job_id) and kind=sqlc.arg(kind)
  and message_id is null
for update;

-- name: GetAction :one
select id,job_id,coalesce(message_id,'') as message_id,kind,state,
       coalesce(external_id,'') as external_id,
       coalesce(external_outcome,'') as external_outcome,scope_key
from dorf.actions
where id=sqlc.arg(id) and job_id=sqlc.arg(job_id) and kind=sqlc.arg(kind);

-- name: GetActionStateForUpdate :one
select state
from dorf.actions
where id=sqlc.arg(id) and job_id=sqlc.arg(job_id) and kind=sqlc.arg(kind)
for update;

-- name: InsertScopedAction :execrows
insert into dorf.actions(id,job_id,kind,state,scope_key)
values(sqlc.arg(id),sqlc.arg(job_id),sqlc.arg(kind),'pending',sqlc.arg(scope_key))
on conflict do nothing;

-- name: GetSetupActionForUpdate :one
select a.job_id,a.kind,coalesce(j.setup_action_id,'') as setup_action_id
from dorf.actions a
join dorf.jobs j on j.id=a.job_id
where a.id=sqlc.arg(action_id)
for update of a,j;

-- name: FinishSetupAction :exec
update dorf.actions
set state=sqlc.arg(state),external_id=sqlc.arg(external_id),
    external_outcome=sqlc.arg(external_outcome)
where dorf.actions.id=sqlc.arg(action_id);

-- name: CompleteAction :one
update dorf.actions
set state='succeeded',external_id=sqlc.arg(external_id),
    external_outcome=nullif(sqlc.arg(external_outcome)::text,'')
where id=sqlc.arg(id)
returning job_id,kind,scope_key;

-- name: MarkActionUncertain :exec
update dorf.actions
set state='uncertain'
where id=sqlc.arg(action_id) and state<>'succeeded';

-- name: MarkTurnActionSucceeded :exec
update dorf.actions
set state='succeeded',external_id=sqlc.arg(turn_id),external_outcome=sqlc.arg(outcome)
where id=sqlc.arg(action_id);

-- name: ResetTurnActionForSubmission :execrows
update dorf.actions
set state='pending'
where id=(select ar.action_id from dorf.agent_runs ar where ar.id=sqlc.arg(run_id))
  and state in ('pending','uncertain');

-- name: MarkRunActionFailed :exec
update dorf.actions
set state=case when exists(
        select 1 from dorf.agent_runs ar
        where ar.id=sqlc.arg(run_id) and ar.native_turn_id is null
    ) then 'uncertain' else 'failed' end,
    external_outcome=sqlc.arg(reason)
where dorf.actions.id=sqlc.arg(action_id);

-- name: MarkRunActionUncertain :exec
update dorf.actions
set state='uncertain',external_outcome=sqlc.arg(reason)
where id=sqlc.arg(action_id) and state<>'succeeded';

-- name: GetReviewSandboxReceiptIdentity :one
select sandbox_name,revision
from dorf.review_resources
where run_id=sqlc.arg(run_id) and sandbox_create_action_id=sqlc.arg(action_id);

-- name: MarkReviewSandboxCreated :execrows
update dorf.review_resources
set sandbox_state='created'
where run_id=sqlc.arg(run_id) and sandbox_state in ('pending','created');

-- name: MarkMainSandboxCreated :execrows
update dorf.sandboxes
set state='created',observed_at=clock_timestamp()
where job_id=sqlc.arg(job_id) and action_id=sqlc.arg(action_id)
  and incus_name=sqlc.arg(incus_name) and state in ('pending','created');

-- name: GetReviewRouteSandbox :one
select sandbox_name
from dorf.review_resources
where run_id=sqlc.arg(run_id) and route_create_action_id=sqlc.arg(action_id);

-- name: MarkReviewRouteActive :execrows
update dorf.review_resources
set route_id=coalesce(route_id,sqlc.arg(route_id)),route_state='active'
where run_id=sqlc.arg(run_id) and sandbox_state='created'
  and route_state in ('pending','active')
  and (route_id is null or route_id=sqlc.arg(route_id));

-- name: MarkMainRouteActive :execrows
update dorf.routes
set state='active',observed_at=clock_timestamp()
where job_id=sqlc.arg(job_id) and action_id=sqlc.arg(action_id)
  and route_id=sqlc.arg(route_id) and state in ('pending','active');

-- name: ImplementationSessionExists :one
select exists(select 1 from dorf.sessions where native_session_id=sqlc.arg(session_id));

-- name: GetReviewControllerIdentity :one
select sandbox_name,ownership_nonce
from dorf.review_resources
where run_id=sqlc.arg(run_id);

-- name: BindReviewAppServer :execrows
update dorf.review_resources
set app_server_id=coalesce(app_server_id,sqlc.arg(app_server_id))
where run_id=sqlc.arg(run_id) and sandbox_state='created' and route_state='active'
  and checkout_state='verified'
  and (app_server_id is null or app_server_id=sqlc.arg(app_server_id));

-- name: UpsertImplementationSession :execrows
insert into dorf.sessions(job_id,action_id,native_session_id)
values(sqlc.arg(job_id),sqlc.arg(action_id),sqlc.arg(session_id))
on conflict(job_id) do update
set native_session_id=excluded.native_session_id,observed_at=clock_timestamp()
where dorf.sessions.native_session_id=excluded.native_session_id;

-- name: GetReviewRouteForCleanup :one
select route_id
from dorf.review_resources
where run_id=sqlc.arg(run_id) and route_revoke_action_id=sqlc.arg(action_id);

-- name: MarkReviewRouteRevoked :execrows
update dorf.review_resources
set route_state='revoked',route_revoked_at=coalesce(route_revoked_at,clock_timestamp())
where run_id=sqlc.arg(run_id) and route_state in ('pending','active','revoked');

-- name: GetMainRouteID :one
select route_id
from dorf.routes
where job_id=sqlc.arg(job_id);

-- name: MarkMainRouteRevoked :execrows
update dorf.routes
set state='revoked',observed_at=clock_timestamp()
where job_id=sqlc.arg(job_id) and route_id=sqlc.arg(route_id)
  and state in ('pending','active','revoked');

-- name: GetReviewSandboxForCleanup :one
select sandbox_name
from dorf.review_resources
where run_id=sqlc.arg(run_id) and sandbox_delete_action_id=sqlc.arg(action_id);

-- name: MarkReviewSandboxDeleted :execrows
update dorf.review_resources
set sandbox_state='deleted',sandbox_deleted_at=coalesce(sandbox_deleted_at,clock_timestamp())
where run_id=sqlc.arg(run_id) and route_state='revoked'
  and sandbox_state in ('pending','created','deleted');

-- name: GetMainSandboxName :one
select incus_name
from dorf.sandboxes
where job_id=sqlc.arg(job_id);

-- name: MarkMainSandboxDeleted :execrows
update dorf.sandboxes
set state='deleted',observed_at=clock_timestamp()
where job_id=sqlc.arg(job_id) and incus_name=sqlc.arg(incus_name)
  and state in ('pending','created','deleted');

-- name: GetReviewWorkspaceReceiptIdentity :one
select ar.workspace,rr.revision
from dorf.review_resources rr
join dorf.agent_runs ar on ar.id=rr.run_id
where rr.materialize_action_id=sqlc.arg(action_id) and rr.run_id=sqlc.arg(run_id);

-- name: MarkReviewCheckoutVerified :execrows
update dorf.review_resources
set checkout_state='verified',revision_tree=coalesce(revision_tree,sqlc.arg(tree)),
    checkout_verified_at=coalesce(checkout_verified_at,clock_timestamp())
where run_id=sqlc.arg(run_id) and sandbox_state='created'
  and checkout_state in ('pending','verified')
  and (revision_tree is null or revision_tree=sqlc.arg(tree));

-- name: ListActions :many
select a.id,coalesce(a.message_id,'') as message_id,a.kind,a.state,
       coalesce(a.external_id,'') as external_id,a.scope_key,
       coalesce(e.digest,'') as evidence_digest
from dorf.actions a
left join dorf.evidence e on e.action_id=a.id
where a.job_id=sqlc.arg(job_id)
order by a.created_at,a.id;
