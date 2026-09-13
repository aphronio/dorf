alter table dorf.job_messages add column developer_instructions text;
insert into dorf.schema_migrations(name) values ('011_message_developer_instructions.sql');
