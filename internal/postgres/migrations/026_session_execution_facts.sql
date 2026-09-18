-- Application policy is no longer part of a Session or Message execution.
alter table dorf.sessions drop column workflow_name, drop column workflow_revision;
create index sessions_by_admitted_at_id on dorf.sessions(admitted_at desc,id desc);

alter table dorf.agent_runs drop column role, drop column capability, drop column input_revision, drop column submission_nonce;

alter table dorf.sessions rename column workflow_attention to execution_attention;
alter table dorf.sessions rename column workflow_attention_source to execution_attention_source;
alter table dorf.sessions rename column workflow_attention_at to execution_attention_at;
alter table dorf.sessions rename constraint jobs_workflow_attention_check to sessions_execution_attention_check;

alter table dorf.session_messages drop constraint job_messages_from_kind_check;
alter table dorf.session_messages add constraint session_messages_from_kind_check
    check (from_kind in ('human','agent'));

insert into dorf.schema_migrations(name) values ('026_session_execution_facts.sql');
