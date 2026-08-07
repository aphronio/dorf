alter table dorf.review_workspaces drop constraint if exists review_workspaces_path_key;

create table if not exists dorf.review_resources (
    run_id text primary key references dorf.agent_runs(id),
    job_id text not null references dorf.jobs(id),
    revision text not null check (revision ~ '^[0-9a-f]{40}([0-9a-f]{24})?$'),
    sandbox_name text not null unique,
    ownership_nonce text not null unique check (ownership_nonce ~ '^[0-9a-f]{64}$'),
    submission_nonce text not null unique check (submission_nonce ~ '^[0-9a-f]{64}$'),
    input_digest text not null check (input_digest ~ '^[0-9a-f]{64}$'),
    sandbox_create_action_id text not null unique references dorf.actions(id),
    route_create_action_id text not null unique references dorf.actions(id),
    materialize_action_id text not null unique references dorf.actions(id),
    route_revoke_action_id text not null unique references dorf.actions(id),
    sandbox_delete_action_id text not null unique references dorf.actions(id),
    sandbox_state text not null default 'pending' check (sandbox_state in ('pending','created','deleted')),
    route_state text not null default 'pending' check (route_state in ('pending','active','revoked')),
    checkout_state text not null default 'pending' check (checkout_state in ('pending','verified')),
    post_review_state text not null default 'pending' check (post_review_state in ('pending','verified')),
    route_id text unique,
    app_server_id text unique,
    revision_tree text check (revision_tree is null or revision_tree ~ '^[0-9a-f]{40}([0-9a-f]{24})?$'),
    checkout_verified_at timestamptz,
    post_review_verified_at timestamptz,
    route_revoked_at timestamptz,
    sandbox_deleted_at timestamptz
);

comment on table dorf.review_resources is
    'Host-owned per-AgentRun Incus Sandbox, provider route, strict native submission, immutable checkout, and exact cleanup facts';
