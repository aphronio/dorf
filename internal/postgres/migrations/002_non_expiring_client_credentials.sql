alter table dorf.control_clients alter column credential_expires_at drop not null;

insert into dorf.schema_migrations(name) values ('002_non_expiring_client_credentials.sql');
