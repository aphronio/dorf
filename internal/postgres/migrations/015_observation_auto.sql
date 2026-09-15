alter table dorf.job_messages drop constraint observation_follow_text;
alter table dorf.job_messages add constraint observation_text check (
    not observation or (requested_intent in ('follow','auto') and attachments = '[]'::jsonb)
);
insert into dorf.schema_migrations(name) values ('015_observation_auto.sql');
