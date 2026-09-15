-- Logical workstation identity survives provider VM replacement. Preserve the
-- original resource's ownership token and leave unknown locators uninvented.
create table dorf.sandbox_resources (
    id text primary key,
    sandbox_id text not null references dorf.sandboxes(id),
    ownership_nonce text not null unique check (ownership_nonce ~ '^[0-9a-f]{64}$'),
    provider_id text check (provider_id is null or length(provider_id)>0),
    reserved_at timestamptz not null default clock_timestamp(),
    observed_at timestamptz,
    deleted_at timestamptz,
    unique(sandbox_id,id),
    check ((provider_id is null)=(observed_at is null))
);
alter table dorf.sandboxes add column active_resource_id text;
insert into dorf.sandbox_resources(id,sandbox_id,ownership_nonce)
select id || ':initial',id,ownership_nonce from dorf.sandboxes;
update dorf.sandboxes set active_resource_id=id || ':initial';
alter table dorf.sandboxes alter column active_resource_id set not null;
alter table dorf.sandboxes add constraint sandboxes_active_owned_resource
    foreign key (id,active_resource_id) references dorf.sandbox_resources(sandbox_id,id)
    deferrable initially deferred;
drop view dorf.review_run_projection;
alter table dorf.sandboxes drop column ownership_nonce;

create view dorf.review_run_projection as
select
    ar.id,
    ar.job_id,
    ar.message_id,
    ar.state,
    coalesce(ar.harness,'') as harness,
    coalesce(ar.thread_id,'') as thread_id,
    (ar.baseline_turn_id is not null)::boolean as baseline_recorded,
    coalesce(ar.baseline_turn_id,'') as baseline_turn_id,
    coalesce(ar.turn_id,'') as turn_id,
    coalesce(ar.turn_outcome,'') as turn_outcome,
    coalesce(ar.attention,'') as attention,
    ar.role,
    coalesce(ar.input_revision,'') as input_revision,
    coalesce(ar.capability,'') as capability,
    ar.started_at,
    ar.finished_at,
    request.from_kind as request_from_kind,
    request.from_id as request_from_id,
    request.sequence as request_sequence,
    request.input as request_input,
    request.delivery_intent as request_delivery_intent,
    coalesce(request.steer_target_turn_id,'') as request_target_turn_id,
    request.admitted_at as request_admitted_at,
    ar.sandbox_id as sandbox_id,
    sandbox.name as sandbox_name,
    resource.ownership_nonce as ownership_nonce,
    coalesce(ar.submission_nonce,'') as submission_nonce,
    sandbox.active_resource_id,
    coalesce(resource.provider_id,'') as provider_id
from dorf.agent_runs ar
join dorf.job_messages request on request.id=ar.message_id and request.job_id=ar.job_id
join dorf.sandboxes sandbox on sandbox.id=ar.sandbox_id
join dorf.sandbox_resources resource on resource.id=sandbox.active_resource_id;

insert into dorf.schema_migrations(name) values ('017_sandbox_resources.sql');
