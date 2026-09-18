-- Retire investigation's application source. Generic Job, Message, and resource
-- receipts remain intact; deployments retire investigation work before upgrading.
drop table dorf.codebase_investigation_sources;

insert into dorf.schema_migrations(name) values ('022_remove_investigation.sql');
