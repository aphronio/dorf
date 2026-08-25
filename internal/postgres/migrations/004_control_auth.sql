create table dorf.control_clients (
    id text primary key check (id ~ '^cli_[A-Za-z0-9_-]{22}$'),
    name text not null check (name ~ '^[a-z0-9][a-z0-9._-]{0,62}$'),
    credential_digest bytea not null unique check (octet_length(credential_digest)=32),
    credential_expires_at timestamptz not null,
    revoked_at timestamptz,
    created_at timestamptz not null default clock_timestamp(),
    check (credential_expires_at>created_at),
    check (revoked_at is null or revoked_at>=created_at)
);

create table dorf.control_enrollments (
    id text primary key check (id ~ '^enr_[A-Za-z0-9_-]{22}$'),
    secret_digest bytea not null unique check (octet_length(secret_digest)=32),
    expires_at timestamptz not null,
    consumed_at timestamptz,
    client_id text unique references dorf.control_clients(id),
    created_at timestamptz not null default clock_timestamp(),
    check (expires_at>created_at),
    check (consumed_at is null or consumed_at>=created_at),
    check (num_nonnulls(consumed_at,client_id) in (0,2))
);

insert into dorf.schema_migrations(name) values ('004_control_auth.sql');
