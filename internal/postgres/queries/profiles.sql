-- name: InsertSandboxProfile :execrows
insert into dorf.sandbox_profiles(
    name,provider,harness,artifact,incus_network,incus_disk_size,
    e2b_gateway_url,e2b_sandbox_timeout_seconds,e2b_allow_internet
)
values(
    sqlc.arg(name),sqlc.arg(provider),sqlc.arg(harness),sqlc.arg(artifact),
    sqlc.arg(incus_network),sqlc.arg(incus_disk_size),sqlc.arg(e2b_gateway_url),
    sqlc.arg(e2b_sandbox_timeout_seconds),sqlc.arg(e2b_allow_internet)
)
on conflict(name) do nothing;

-- name: GetSandboxProfile :one
select p.name,p.provider,p.harness,p.artifact,
       coalesce(p.incus_network,'') as incus_network,
       coalesce(p.incus_disk_size,'') as incus_disk_size,
       coalesce(p.e2b_gateway_url,'') as e2b_gateway_url,
       coalesce(p.e2b_sandbox_timeout_seconds,0) as e2b_sandbox_timeout_seconds,
       coalesce(p.e2b_allow_internet,false) as e2b_allow_internet,
       p.is_default,p.created_at,
       coalesce(v.contract_version,'') as verification_contract,
       coalesce(v.sandbox_id,'') as verification_sandbox_id,
       coalesce(v.ownership_nonce,'') as verification_ownership_nonce,
       coalesce(v.harness_version,'') as verification_harness_version,
       v.attempted_at,v.probe_completed_at,v.cleaned_at,
       coalesce(v.last_error,'') as verification_last_error
from dorf.sandbox_profiles p
left join dorf.sandbox_profile_verifications v on v.profile_name=p.name
where p.name=sqlc.arg(name);

-- name: ListSandboxProfiles :many
select p.name,p.provider,p.harness,p.artifact,
       coalesce(p.incus_network,'') as incus_network,
       coalesce(p.incus_disk_size,'') as incus_disk_size,
       coalesce(p.e2b_gateway_url,'') as e2b_gateway_url,
       coalesce(p.e2b_sandbox_timeout_seconds,0) as e2b_sandbox_timeout_seconds,
       coalesce(p.e2b_allow_internet,false) as e2b_allow_internet,
       p.is_default,p.created_at,
       coalesce(v.contract_version,'') as verification_contract,
       coalesce(v.sandbox_id,'') as verification_sandbox_id,
       coalesce(v.ownership_nonce,'') as verification_ownership_nonce,
       coalesce(v.harness_version,'') as verification_harness_version,
       v.attempted_at,v.probe_completed_at,v.cleaned_at,
       coalesce(v.last_error,'') as verification_last_error
from dorf.sandbox_profiles p
left join dorf.sandbox_profile_verifications v on v.profile_name=p.name
order by p.name;

-- name: GetDefaultSandboxProfile :one
select name from dorf.sandbox_profiles where is_default;

-- name: LockVerifiedSandboxProfileForAdmission :one
select p.name
from dorf.sandbox_profiles p
join dorf.sandbox_profile_verifications v on v.profile_name=p.name
where p.name=sqlc.arg(name) and v.contract_version=sqlc.arg(contract_version)
  and v.probe_completed_at is not null and v.cleaned_at is not null and v.last_error is null
for key share of p;

-- name: LockSandboxProfile :one
select name,provider,harness,artifact,
       coalesce(incus_network,'') as incus_network,
       coalesce(incus_disk_size,'') as incus_disk_size,
       coalesce(e2b_gateway_url,'') as e2b_gateway_url,
       coalesce(e2b_sandbox_timeout_seconds,0) as e2b_sandbox_timeout_seconds,
       coalesce(e2b_allow_internet,false) as e2b_allow_internet,
       is_default,created_at
from dorf.sandbox_profiles where name=sqlc.arg(name) for update;

-- name: ProfileHasIncompleteJobs :one
select exists(
    select 1 from dorf.jobs where sandbox_profile=sqlc.arg(name) and cleanup_state<>'complete'
) as in_use;

-- name: ProfileVerificationNeedsCleanup :one
select exists(
    select 1 from dorf.sandbox_profile_verifications
    where profile_name=sqlc.arg(profile_name) and cleaned_at is null
) as needs_cleanup;

-- name: UpdateSandboxProfile :execrows
update dorf.sandbox_profiles
set provider=sqlc.arg(provider),harness=sqlc.arg(harness),artifact=sqlc.arg(artifact),
    incus_network=sqlc.arg(incus_network),incus_disk_size=sqlc.arg(incus_disk_size),
    e2b_gateway_url=sqlc.arg(e2b_gateway_url),
    e2b_sandbox_timeout_seconds=sqlc.arg(e2b_sandbox_timeout_seconds),
    e2b_allow_internet=sqlc.arg(e2b_allow_internet),is_default=false
where name=sqlc.arg(name);

-- name: DeleteProfileVerification :exec
delete from dorf.sandbox_profile_verifications where profile_name=sqlc.arg(profile_name);

-- name: ClearDefaultSandboxProfile :exec
update dorf.sandbox_profiles set is_default=false where is_default;

-- name: SetDefaultSandboxProfile :execrows
update dorf.sandbox_profiles set is_default=true where name=sqlc.arg(name);

-- name: BeginSandboxProfileVerification :one
insert into dorf.sandbox_profile_verifications(
    profile_name,contract_version,sandbox_id,ownership_nonce
)
values(sqlc.arg(profile_name),sqlc.arg(contract_version),sqlc.arg(sandbox_id),sqlc.arg(ownership_nonce))
on conflict(profile_name) do update
set attempted_at=clock_timestamp(),cleaned_at=null,last_error=null
where dorf.sandbox_profile_verifications.probe_completed_at is null
returning profile_name,contract_version,sandbox_id,ownership_nonce,
          coalesce(harness_version,'') as harness_version,attempted_at,
          probe_completed_at,cleaned_at,coalesce(last_error,'') as last_error;

-- name: RecordSandboxProfileProbe :execrows
update dorf.sandbox_profile_verifications
set harness_version=sqlc.arg(harness_version),probe_completed_at=coalesce(probe_completed_at,clock_timestamp()),last_error=null
where profile_name=sqlc.arg(profile_name) and contract_version=sqlc.arg(contract_version)
  and sandbox_id=sqlc.arg(sandbox_id) and ownership_nonce=sqlc.arg(ownership_nonce);

-- name: RecordSandboxProfileVerificationCleanup :execrows
update dorf.sandbox_profile_verifications
set cleaned_at=coalesce(cleaned_at,clock_timestamp()),last_error=null
where profile_name=sqlc.arg(profile_name) and contract_version=sqlc.arg(contract_version)
  and sandbox_id=sqlc.arg(sandbox_id) and ownership_nonce=sqlc.arg(ownership_nonce);

-- name: RecordSandboxProfileVerificationError :execrows
update dorf.sandbox_profile_verifications
set last_error=sqlc.arg(last_error)
where profile_name=sqlc.arg(profile_name) and contract_version=sqlc.arg(contract_version)
  and sandbox_id=sqlc.arg(sandbox_id) and ownership_nonce=sqlc.arg(ownership_nonce);

-- name: MarkSandboxProfileUnavailable :execrows
update dorf.sandbox_profile_verifications
set last_error=sqlc.arg(last_error)
where profile_name=sqlc.arg(profile_name)
  and contract_version=sqlc.arg(contract_version)
  and probe_completed_at is not null and cleaned_at is not null;
