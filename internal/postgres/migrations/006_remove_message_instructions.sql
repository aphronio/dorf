alter table dorf.job_messages drop column instructions;

insert into dorf.schema_migrations(name) values ('006_remove_message_instructions.sql');
