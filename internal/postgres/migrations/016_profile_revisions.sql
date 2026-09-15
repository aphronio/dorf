-- A profile name selects a candidate and a verified active revision. Jobs pin
-- the exact definition at admission; promotion never changes their custody.
alter table dorf.jobs drop constraint jobs_sandbox_profile_fkey;
alter table dorf.sandbox_profile_verifications drop constraint sandbox_profile_verifications_profile_name_fkey;
alter table dorf.sandbox_profiles rename to sandbox_profile_revisions;
drop index dorf.sandbox_profiles_one_default;
alter table dorf.sandbox_profile_revisions drop constraint sandbox_profiles_pkey;
alter table dorf.sandbox_profile_revisions add primary key(name,definition_hash);

create table dorf.sandbox_profiles (
    name text primary key check (name ~ '^[a-z][a-z0-9-]{0,62}$'),
    candidate_revision text not null,
    active_revision text,
    is_default boolean not null default false,
    created_at timestamptz not null default clock_timestamp(),
    foreign key(name,candidate_revision) references dorf.sandbox_profile_revisions(name,definition_hash),
    foreign key(name,active_revision) references dorf.sandbox_profile_revisions(name,definition_hash)
);
create unique index sandbox_profiles_one_default on dorf.sandbox_profiles(is_default) where is_default;
insert into dorf.sandbox_profiles(name,candidate_revision,active_revision,is_default,created_at)
select p.name,p.definition_hash,
       case when v.probe_completed_at is not null and v.cleaned_at is not null and v.last_error is null
            then p.definition_hash end,p.is_default,p.created_at
from dorf.sandbox_profile_revisions p
left join dorf.sandbox_profile_verifications v on v.profile_name=p.name;
alter table dorf.sandbox_profile_revisions drop column is_default;

alter table dorf.sandbox_profile_verifications drop constraint sandbox_profile_verifications_pkey;
alter table dorf.sandbox_profile_verifications add primary key(profile_name,definition_hash);
alter table dorf.sandbox_profile_verifications add foreign key(profile_name,definition_hash)
    references dorf.sandbox_profile_revisions(name,definition_hash);

alter table dorf.jobs add column sandbox_profile_revision text;
update dorf.jobs j set sandbox_profile_revision=p.candidate_revision
from dorf.sandbox_profiles p where p.name=j.sandbox_profile;
alter table dorf.jobs alter column sandbox_profile_revision set not null;
alter table dorf.jobs add foreign key(sandbox_profile,sandbox_profile_revision)
    references dorf.sandbox_profile_revisions(name,definition_hash);

-- Protect exact definitions and accepted bindings from accidental in-place edits.
create function dorf.immutable_profile_revision() returns trigger language plpgsql as $$
begin
    raise exception 'Sandbox profile revisions are immutable';
end;
$$;
create trigger immutable_profile_revision before update on dorf.sandbox_profile_revisions
for each row execute function dorf.immutable_profile_revision();
create function dorf.immutable_job_profile_revision() returns trigger language plpgsql as $$
begin
    if new.sandbox_profile is distinct from old.sandbox_profile or
       new.sandbox_profile_revision is distinct from old.sandbox_profile_revision then
        raise exception 'Job Sandbox profile binding is immutable';
    end if;
    return new;
end;
$$;
create trigger immutable_job_profile_revision before update of sandbox_profile,sandbox_profile_revision on dorf.jobs
for each row execute function dorf.immutable_job_profile_revision();
insert into dorf.schema_migrations(name) values ('016_profile_revisions.sql');
