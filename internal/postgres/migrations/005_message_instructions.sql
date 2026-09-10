alter table dorf.job_messages add column instructions text;

insert into dorf.schema_migrations(name) values ('005_message_instructions.sql');
