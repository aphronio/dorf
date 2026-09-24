alter table dorf.sandbox_checkpoints add column id text not null default gen_random_uuid()::text unique;
insert into dorf.schema_migrations(name) values ('031_checkpoint_identity.sql');
