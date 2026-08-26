-- Bind profile verification to the exact provider definition and move Incus
-- endpoint identity into Deployment custody. Existing profile rows remain
-- readable but deliberately lack the hashes and Incus routing facts required
-- for new admission until an operator explicitly updates and re-verifies them.

alter table dorf.sandbox_profiles
    add column if not exists definition_hash text,
    add column if not exists incus_endpoint_authority_hash text,
    add column if not exists incus_project text,
    add column if not exists incus_storage_pool text,
    add column if not exists incus_gateway_url text;

alter table dorf.sandbox_profiles drop constraint if exists sandbox_profiles_check;
alter table dorf.sandbox_profiles add constraint sandbox_profiles_check check (
    (
        provider='incus' and incus_network is not null and incus_disk_size is not null and
        length(trim(incus_network)) > 0 and length(trim(incus_disk_size)) > 0 and
        e2b_gateway_url is null and e2b_sandbox_timeout_seconds is null and e2b_allow_internet is null and
        (
            (definition_hash is null and incus_endpoint_authority_hash is null and incus_project is null and
             incus_storage_pool is null and incus_gateway_url is null) or
            (definition_hash ~ '^[0-9a-f]{64}$' and incus_endpoint_authority_hash ~ '^[0-9a-f]{64}$' and
             length(trim(incus_project)) > 0 and length(trim(incus_storage_pool)) > 0 and
             length(trim(incus_gateway_url)) > 0)
        )
    ) or
    (
        provider='e2b' and incus_network is null and incus_disk_size is null and
        incus_endpoint_authority_hash is null and incus_project is null and incus_storage_pool is null and
        incus_gateway_url is null and e2b_gateway_url is not null and
        e2b_sandbox_timeout_seconds is not null and length(trim(e2b_gateway_url)) > 0 and
        e2b_sandbox_timeout_seconds > 0 and e2b_allow_internet is not null and
        (definition_hash is null or definition_hash ~ '^[0-9a-f]{64}$')
    )
);

alter table dorf.sandbox_profile_verifications
    add column if not exists definition_hash text;
alter table dorf.sandbox_profile_verifications
    drop constraint if exists sandbox_profile_verifications_definition_hash_check;
alter table dorf.sandbox_profile_verifications
    add constraint sandbox_profile_verifications_definition_hash_check
    check (definition_hash is null or definition_hash ~ '^[0-9a-f]{64}$');

insert into dorf.schema_migrations(name) values ('007_profile_endpoint_custody.sql');
