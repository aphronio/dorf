alter table dorf.job_messages add column observation boolean not null default false;
alter table dorf.job_messages add constraint observation_follow_text check (not observation or (delivery_intent = 'follow' and requested_intent = 'follow' and attachments = '[]'::jsonb));
insert into dorf.schema_migrations(name) values ('013_message_observation.sql');
