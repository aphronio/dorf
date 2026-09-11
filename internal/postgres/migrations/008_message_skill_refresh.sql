alter table dorf.job_messages
    add column refresh_skills boolean not null default false;

insert into dorf.schema_migrations(name) values ('008_message_skill_refresh.sql');
